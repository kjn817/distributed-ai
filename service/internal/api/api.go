// Package api wires the HTTP routes: OpenAI-compatible inference API,
// node registration, and the network dashboard API.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"distributed-ai/engine"
	jobprotocol "distributed-ai/protocol"
	"distributed-ai/service/internal/credit"
	"distributed-ai/service/internal/dashboard"
	"distributed-ai/service/internal/dispatch"
	"distributed-ai/service/internal/metrics"
	"distributed-ai/service/internal/placement"
	"distributed-ai/service/internal/quest"
	"distributed-ai/service/internal/registry"
	"distributed-ai/service/internal/replica"
)

// Server holds dependencies.
type Server struct {
	reg     *registry.Registry
	eng     engine.Engine // nil when no local engine is attached
	p2phost host.Host     // nil until the service libp2p host is attached
	ledger  credit.Ledger // nil until attached
	keys    *credit.APIKeys
	// challenges backs the peer-ID claim flow (node operators proving
	// ownership to get an API key for their earnings account).
	challenges *credit.Challenges
	metrics    *metrics.Tracker
	// founder receives the 1% maintenance share; "" = disabled.
	// Disabled by default — enabled only with explicit user approval
	// (FOUNDER_SHARE_ENABLED=1).
	founder      string
	founderShare bool
}

// New creates the API server.
func New(reg *registry.Registry) *Server {
	return &Server{reg: reg, metrics: metrics.New(), keys: credit.NewAPIKeys(), challenges: credit.NewChallenges()}
}

// SetFounder configures the founder account for the 1% maintenance share.
// It takes effect only when enableShare is true (explicit user approval).
func (s *Server) SetFounder(accountID string, enableShare bool) {
	s.founder = accountID
	s.founderShare = enableShare && accountID != ""
}

// Metrics exposes the demand tracker for the replica planner.
func (s *Server) Metrics() *metrics.Tracker { return s.metrics }

// WithEngine attaches a local inference engine for /v1/chat/completions.
func (s *Server) WithEngine(e engine.Engine) *Server {
	s.eng = e
	return s
}

// WithP2PHost attaches the service libp2p host (remote job dispatch).
func (s *Server) WithP2PHost(h host.Host) *Server {
	s.p2phost = h
	return s
}

// Ledger exposes the credit ledger (for TEST_FUND at startup).
func (s *Server) Ledger() credit.Ledger { return s.ledger }

// APIKeys exposes the API key store (for TEST_FUND at startup).
func (s *Server) APIKeys() *credit.APIKeys { return s.keys }

// WithLedger attaches the credit ledger (volunteer earnings).
func (s *Server) WithLedger(l credit.Ledger) *Server {
	s.ledger = l
	return s
}

// Routes registers all handlers.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/nodes/register", s.handleNodeRegister)
	mux.HandleFunc("POST /v1/nodes/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /", dashboard.Handler().ServeHTTP)
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, dashboard.VersionInfo())
	})
	mux.HandleFunc("GET /network/status", s.handleNetworkStatus)
	mux.HandleFunc("GET /v1/credits/{account}", s.handleCreditBalance)
	mux.HandleFunc("GET /v1/credits/{account}/history", s.handleCreditHistory)
	mux.HandleFunc("POST /v1/accounts", s.handleCreateAccount)
	mux.HandleFunc("POST /v1/accounts/claim", s.handleClaimChallenge)
	mux.HandleFunc("POST /v1/accounts/claim/verify", s.handleClaimVerify)
	mux.HandleFunc("GET /v1/models/{id}/manifest", s.handleModelManifest)
	mux.HandleFunc("GET /v1/models/{id}/holders", s.handleModelHolders)
	mux.HandleFunc("GET /v1/quest", s.handleQuest)
	mux.HandleFunc("GET /v1/metrics", s.handleMetrics)
	mux.HandleFunc("GET /v1/replicas", s.handleReplicas)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.reg.Models()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ModelID,
			"object":   "model",
			"status":   m.Status,
			"owned_by": "distributed-ai",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// chatRequest is the subset of the OpenAI chat format we route on.
// Spending is authenticated via the Authorization: Bearer <api-key> header;
// the key's account pays. There is no `user` field — a client cannot name
// an arbitrary account to spend from.
type chatRequest struct {
	Model     string                    `json:"model"`
	Messages  []jobprotocol.ChatMessage `json:"messages"`
	MaxTokens int                       `json:"max_tokens"`
	// Distributed requests joint compute: one request computed by a
	// coordinator + RPC workers (vs request routing to a single node).
	Distributed bool `json:"distributed,omitempty"`
}

// maxTokensOrDefault bounds generation length (OpenAI-compatible default).
func maxTokensOrDefault(n int) int {
	if n <= 0 {
		return 256
	}
	return n
}

// handleChatCompletions routes via compute placement: single-node-first
// (best verified remote node, else local engine), honest 503 when nothing
// can serve, honest 501 when the model's execution strategy is not
// implemented yet.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty request body"})
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid chat request"})
		return
	}

	// Credit auth: spending requires a valid API key. The key's account
	// pays; there is no way to name another account.
	// Auth runs BEFORE placement: no capacity info leaks to strangers,
	// and no work starts unauthenticated.
	var spender string
	var reservation string
	if s.ledger != nil {
		acct, ok := s.authenticate(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{
					"message": "valid API key required (Authorization: Bearer <key>)",
					"type":    "auth_required",
				},
			})
			return
		}
		spender = acct
		// Reserve an upper bound up front (atomic check-and-hold), so two
		// concurrent requests cannot double-spend the same balance.
		// Estimate: prompt chars/4 + maxTokens, with 2x headroom.
		promptChars := 0
		for _, m := range req.Messages {
			promptChars += len(m.Content)
		}
		upperBound := int64(promptChars/4+maxTokensOrDefault(req.MaxTokens)) * 2 * credit.CreditsPerToken
		if upperBound < 64 {
			upperBound = 64 // floor: never free
		}
		res, ok := s.ledger.Reserve(acct, upperBound)
		if !ok {
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error": map[string]any{
					"message": "insufficient credits",
					"type":    "insufficient_credits",
				},
			})
			return
		}
		reservation = res
	}

	strategy := placement.StrategyFullMaterialize // default when the manifest is unknown
	if mf, ok := s.reg.Manifest(req.Model); ok && len(mf.Execution.Strategies) > 0 {
		strategy = mf.Execution.Strategies[0]
	}
	// Joint compute vs request routing: the client asks for distributed,
	// or the manifest declares DISTRIBUTED_MATERIALIZE.
	var tgt *placement.Assignment
	if req.Distributed || strategy == placement.StrategyDistributedMaterialize {
		tgt, err = placement.DistributedGroup{MinWorkers: 1}.Place(
			s.reg, s.eng != nil && s.eng.Ready(),
			placement.Request{ModelID: req.Model, Strategy: strategy},
		)
	} else {
		tgt, err = placement.SingleNodeFirst{}.Place(
			s.reg, s.eng != nil && s.eng.Ready(),
			placement.Request{ModelID: req.Model, Strategy: strategy},
		)
	}
	if err != nil {
		// No work will happen — release the hold.
		if s.ledger != nil && reservation != "" {
			s.ledger.Release(reservation)
		}
		if errors.Is(err, placement.ErrStrategyUnsupported) {
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"error": map[string]any{
					"message": "model execution strategy not supported yet: " + strategy,
					"type":    "strategy_unsupported",
				},
			})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"message": "no inference capacity right now",
				"type":    "no_capacity",
			},
		})
		return
	}
	s.metrics.Record(req.Model)

	if tgt.Kind == "local" {
		// Replay the already-read body into the proxy.
		// Note: token usage isn't visible through the proxy, so the local
		// path neither earns nor deducts credits (v1) — release the hold.
		if s.ledger != nil && reservation != "" {
			s.ledger.Release(reservation)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		target, err := url.Parse(s.eng.BaseURL())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "bad engine url"})
			return
		}
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
		return
	}

	// Remote: dispatch over /distributed-ai/job/1.
	node := s.reg.Get(tgt.NodeID)
	if node == nil || s.p2phost == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "remote target unavailable", "type": "no_capacity"},
		})
		return
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 256
	}
	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	// Track in-flight load so concurrent requests spread across replicas
	// (placement prefers the capable node with fewest in-flight).
	s.reg.AddInFlight(node.ID, 1)
	defer s.reg.AddInFlight(node.ID, -1)
	jobReq := &jobprotocol.JobRequest{
		JobID:     jobID,
		ModelID:   req.Model,
		Strategy:  "FULL_MATERIALIZE",
		Messages:  req.Messages,
		MaxTokens: maxTokens,
	}
	if tgt.Kind == "distributed" {
		// Joint compute: the coordinator runs llama-server with --rpc
		// backends (the cohort workers). One request, multiple nodes.
		jobReq.Strategy = "DISTRIBUTED"
		for _, m := range tgt.Cohort {
			if m.RpcAddr != "" {
				jobReq.RpcBackends = append(jobReq.RpcBackends, m.RpcAddr)
			}
		}
	}
	res, err := dispatch.Dispatch(ctx, s.p2phost, node, jobReq)
	if err != nil {
		// The work didn't happen — release the hold, don't charge.
		if s.ledger != nil && reservation != "" {
			s.ledger.Release(reservation)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "remote inference failed: " + err.Error(), "type": "remote_error"},
		})
		return
	}
	// The volunteer earned credit for real work: 1 per token.
	// The founder's 1% is disabled by default (explicit approval required).
	// The authenticated spender pays actual tokens via their reservation.
	if s.ledger != nil {
		total := int64(res.PromptTokens+res.GenTokens) * credit.CreditsPerToken
		if s.founderShare {
			founderCut := total * credit.FounderSharePct / 100
			if nodeShare := total - founderCut; nodeShare > 0 {
				_, _ = s.ledger.Earn(node.ID, jobID, node.ID, nodeShare)
			}
			if founderCut > 0 && s.founder != "" {
				_, _ = s.ledger.Earn(s.founder, jobID, node.ID, founderCut)
			}
		} else {
			_, _ = s.ledger.Earn(node.ID, jobID, node.ID, total)
		}
		if reservation != "" {
			_ = s.ledger.Commit(reservation, total)
		} else if spender != "" {
			_, _ = s.ledger.SpendUpTo(spender, jobID, total)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + jobID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": res.Content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     res.PromptTokens,
			"completion_tokens": res.GenTokens,
			"total_tokens":      res.PromptTokens + res.GenTokens,
		},
	})
}

// handleModelManifest serves the full manifest (chunk hashes are the trust
// root for P2P transfer).
func (s *Server) handleModelManifest(w http.ResponseWriter, r *http.Request) {
	m, ok := s.reg.Manifest(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown model"})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleModelHolders lists online nodes serving the model — chunk sources
// for materialization.
func (s *Server) handleModelHolders(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	holders := s.reg.ModelHolders(id)
	out := make([]map[string]any, 0, len(holders))
	for _, n := range holders {
		out = append(out, map[string]any{
			"peer_id":   n.ID,
			"addrs":     n.Addrs,
			"source_ip": n.SourceIP,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"model_id": id, "holders": out})
}

// handleReplicas serves per-model replication health: actual holders vs
// desired, from live counts.
func (s *Server) handleReplicas(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": replica.Statuses(s.reg, s.metrics)})
}

// handleMetrics serves per-model demand stats (real served requests).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": s.metrics.Snapshot()})
}

// handleQuest serves the Model Quest ladder: per-model unlock state and
// progress, derived from live resource snapshots.
func (s *Server) handleQuest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ladder": quest.Evaluate(s.reg)})
}

// handleCreditBalance reports an account's credit balance (earned by serving
// jobs). v1: the account is the node's peer ID.
func (s *Server) handleCreditBalance(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ledger not attached"})
		return
	}
	account := r.PathValue("account")
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": account,
		"balance":    s.ledger.Balance(account),
	})
}

func (s *Server) handleCreditHistory(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ledger not attached"})
		return
	}
	account := r.PathValue("account")
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": account,
		"entries":    s.ledger.History(account),
	})
}

// handleCreateAccount issues a new credit account and API key.
// The key is shown once; only its hash is stored.
func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ledger not attached"})
		return
	}
	id, key, err := s.keys.CreateAccount()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "key generation failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": id,
		"api_key":    key,
		"warning":    "store this key now — it is never shown again",
	})
}

// authenticate extracts the account ID from a Bearer API key.
func (s *Server) authenticate(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return s.keys.Authenticate(strings.TrimPrefix(h, prefix))
}

// handleClaimChallenge starts the peer-ID ownership proof: the node signs
// the returned challenge with its libp2p private key.
func (s *Server) handleClaimChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PeerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "peer_id required"})
		return
	}
	if s.reg.Get(body.PeerID) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown peer — register the node first"})
		return
	}
	ch, err := s.challenges.Issue(body.PeerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "challenge failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer_id": body.PeerID, "challenge": ch})
}

// handleClaimVerify completes the ownership proof and issues an API key
// for the peer ID account (the node's earnings account).
func (s *Server) handleClaimVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PeerID    string `json:"peer_id"`
		Challenge string `json:"challenge"`
		Signature string `json:"signature"` // base64, over raw challenge bytes
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body.PeerID == "" || body.Challenge == "" || body.Signature == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "peer_id, challenge and signature required"})
		return
	}
	if !s.challenges.Consume(body.PeerID, body.Challenge) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or expired challenge"})
		return
	}
	pid, err := peer.Decode(body.PeerID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid peer ID"})
		return
	}
	pub, err := pid.ExtractPublicKey()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot extract public key"})
		return
	}
	chBytes, err := base64.RawURLEncoding.DecodeString(body.Challenge)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid challenge encoding"})
		return
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(body.Signature)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid signature encoding"})
		return
	}
	ok, err := pub.Verify(chBytes, sigBytes)
	if err != nil || !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "signature verification failed"})
		return
	}
	key, err := credit.NewKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "key generation failed"})
		return
	}
	s.keys.IssueFor(body.PeerID, key)
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": body.PeerID,
		"api_key":    key,
		"warning":    "store this key now — it is never shown again",
	})
}

func (s *Server) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	var n registry.Node
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid node payload"})
		return
	}
	// Capture the HTTP source IP: the node provably reached us from here,
	// so it's the first dial-back candidate (see p2pnet.DialBack).
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		n.SourceIP = host
	}
	// Storage bonus: becoming a NEW holder is a successful storage task
	// (plan §22 — not mere uptime). One-time credit per GB stored.
	old := s.reg.Get(n.ID)
	s.reg.Register(&n)
	if s.ledger != nil {
		had := map[string]bool{}
		if old != nil {
			for _, m := range old.Models {
				had[m] = true
			}
		}
		for _, m := range n.Models {
			if had[m] {
				continue
			}
			if mf, ok := s.reg.Manifest(m); ok && mf.SizeBytes > 0 {
				bonus := mf.SizeBytes / credit.GB * credit.CreditsPerGBStored
				if mf.SizeBytes%credit.GB != 0 {
					bonus++ // round up: a partial GB still stores real bytes
				}
				if bonus > 0 {
					_, _ = s.ledger.Earn(n.ID, "store-"+m, n.ID, bonus)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node_id": n.ID})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NodeID        string `json:"node_id"`
		ChunkReceived []struct {
			PeerID string `json:"peer_id"`
			Bytes  int64  `json:"bytes"`
		} `json:"chunk_received"`
		Limits *struct {
			Enforced  bool   `json:"enforced"`
			Mechanism string `json:"mechanism"`
			Reason    string `json:"reason"`
		} `json:"limits"`
		RpcAddr string `json:"rpc_addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.NodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "node_id required"})
		return
	}
	if !s.reg.Heartbeat(body.NodeID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown node, register first"})
		return
	}
	// OS cap status (honest: enforced or the reason it isn't).
	if body.Limits != nil {
		s.reg.SetLimits(body.NodeID, body.Limits.Enforced, body.Limits.Mechanism, body.Limits.Reason)
	}
	// RPC worker address ("" = not a worker).
	if body.RpcAddr != "" {
		s.reg.SetRpcAddr(body.NodeID, body.RpcAddr)
	}
	// Transfer metering, receiver-confirmed: credit the SENDER on the
	// downloader's report. A node cannot inflate earnings by self-reporting.
	if s.ledger != nil {
		for _, rep := range body.ChunkReceived {
			if pay := s.reg.AddTransferBytes(rep.PeerID, rep.Bytes, credit.GB); pay > 0 {
				gb := pay / credit.GB
				_, _ = s.ledger.Earn(rep.PeerID,
					fmt.Sprintf("xfer-%d", time.Now().UnixNano()),
					rep.PeerID, gb*credit.CreditsPerGBXfer)
			}
		}
	}
	// Deliver pending replication assignments (the planner's healing work).
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"replicate": s.reg.TakeReplications(body.NodeID),
	})
}

func (s *Server) handleNetworkStatus(w http.ResponseWriter, r *http.Request) {
	totals := s.reg.Totals()
	totals["compute_nodes"] = len(s.reg.ComputeNodes())
	totals["nodes"] = s.reg.OnlineNodes()
	writeJSON(w, http.StatusOK, totals)
}
