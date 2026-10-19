// Package executor runs inference jobs assigned over /distributed-ai/job/1.
//
// v1: FULL_MATERIALIZE only, and the model file must be pre-placed
// (MODEL_PATH/MODEL_ID env, same as the service). P2P model distribution
// (M5) removes the pre-placement requirement. If the requested model isn't
// the one loaded, the job fails honestly instead of answering with the
// wrong model.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"

	"distributed-ai/engine"
	"distributed-ai/node/internal/materialize"
	"distributed-ai/protocol"
)

// Executor serves job streams using a local engine.
type Executor struct {
	mu          sync.Mutex
	mgr         *engine.Manager
	modelID     string
	rpcBackends []string
	rpcNgl      int
	// engineCfg is set for preplaced mode (New); allows restart with
	// different RPC backends for joint compute.
	engineCfg *engine.Config
	lazy      *LazyConfig
	client    *http.Client
}

// equalStrings compares backend lists (order matters for --rpc).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// LazyConfig arms on-demand materialization: the first job for an unknown
// model fetches it over P2P chunk transfer instead of failing.
type LazyConfig struct {
	EngineBin    string
	Port         int
	CtxSize      int
	Threads      int   // CPU threads = shared cores (share enforcement)
	CPUPercent   int   // OS CPU cap; Linux cgroup v2
	MemBytes     int64 // OS RAM cap; Linux cgroup v2
	MaxBytes     int64 // SSD cap for models dir (0 = no cap)
	ServiceURL   string
	ModelsDir    string
	Host         host.Host
	OnModelReady func(modelID, path string)
	// OnChunkReceived reports receiver-confirmed bytes per source peer
	// (for transfer credit); may be nil.
	OnChunkReceived func(peerID string, bytes int64)
}

// New creates an Executor around a started engine manager (pre-placed model).
func New(mgr *engine.Manager, modelID string, cfg engine.Config) *Executor {
	return &Executor{mgr: mgr, modelID: modelID, engineCfg: &cfg, client: &http.Client{Timeout: 120 * time.Second}}
}

// NewLazy creates an Executor with no model loaded; the first job triggers
// P2P materialization via cfg.
func NewLazy(cfg LazyConfig) *Executor {
	return &Executor{lazy: &cfg, client: &http.Client{Timeout: 120 * time.Second}}
}

// HandleStream serves one JobRequest per stream.
func (e *Executor) HandleStream(s network.Stream) {
	defer s.Close()
	// Generous deadline: inference takes a while; the dialer gives up via
	// its own context and closes the stream, which fails our writes.
	_ = s.SetDeadline(time.Now().Add(5 * time.Minute))
	var req protocol.JobRequest
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		return // malformed: nothing honest to send back
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res := e.execute(ctx, &req)
	_ = json.NewEncoder(s).Encode(res)
}

func (e *Executor) execute(ctx context.Context, req *protocol.JobRequest) protocol.JobResult {
	start := time.Now()
	fail := func(format string, args ...any) protocol.JobResult {
		return protocol.JobResult{JobID: req.JobID, Error: fmt.Sprintf(format, args...)}
	}
	if req.Strategy != "" && req.Strategy != "FULL_MATERIALIZE" && req.Strategy != "DISTRIBUTED" {
		return fail("unsupported strategy %q", req.Strategy)
	}
	// Joint compute: one request, multiple nodes via llama.cpp RPC.
	// The coordinator runs llama-server with --rpc backends; workers run
	// rpc-server (see --rpc-worker). Distinct from request routing, which
	// sends different requests to different nodes.
	mgr, err := e.ensureModel(ctx, req.ModelID, req.RpcBackends, req.RpcNgl)
	if err != nil {
		return fail("%v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"messages":   req.Messages,
		"max_tokens": req.MaxTokens,
		"stream":     false,
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", mgr.BaseURL()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fail("request build: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return fail("engine call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail("engine http %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fail("engine decode: %v", err)
	}
	if len(out.Choices) == 0 {
		return fail("engine returned no choices")
	}
	return protocol.JobResult{
		JobID:        req.JobID,
		Content:      out.Choices[0].Message.Content,
		PromptTokens: out.Usage.PromptTokens,
		GenTokens:    out.Usage.CompletionTokens,
		DurationMs:   time.Since(start).Milliseconds(),
	}
}

// ensureModel returns a ready engine for modelID, materializing it over P2P
// first when lazy mode is armed. The slow download happens without holding
// the lock; the manager swap is atomic under it.
// rpcNgl: layers to offload to RPC workers (0 = default 99, full offload).
func (e *Executor) ensureModel(ctx context.Context, modelID string, rpcBackends []string, rpcNgl int) (*engine.Manager, error) {
	ngl := rpcNgl
	if ngl <= 0 {
		ngl = 99 // default: full offload for verification
	}
	e.mu.Lock()
	if e.mgr != nil && e.modelID == modelID && e.mgr.Ready() && equalStrings(e.rpcBackends, rpcBackends) && e.rpcNgl == ngl {
		mgr := e.mgr
		e.mu.Unlock()
		return mgr, nil
	}
	lazy := e.lazy
	engCfg := e.engineCfg
	e.mu.Unlock()

	// Preplaced mode with backend change: restart the engine with --rpc.
	if lazy == nil && engCfg != nil && e.modelID == modelID {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.mgr != nil {
			_ = e.mgr.Stop()
		}
		cfg := *engCfg
		if len(rpcBackends) > 0 {
			// Joint compute: --rpc registers backends, -ngl offloads layers.
			// Without -ngl, all layers stay on coordinator CPU (false success).
			cfg.ExtraArgs = []string{"--rpc", strings.Join(rpcBackends, ","), "--n-gpu-layers", fmt.Sprint(ngl)}
		} else {
			cfg.ExtraArgs = nil
		}
		mgr := engine.New(cfg)
		if err := mgr.Start(ctx); err != nil {
			return nil, fmt.Errorf("engine restart: %w", err)
		}
		e.mgr = mgr
		e.rpcBackends = rpcBackends
		e.rpcNgl = ngl
		return mgr, nil
	}

	if lazy == nil {
		if e.modelID == "" {
			return nil, fmt.Errorf("no model loaded on this node")
		}
		return nil, fmt.Errorf("model %q not available on this node (serving %q)", modelID, e.modelID)
	}

	path, err := materialize.Materialize(ctx, lazy.Host, lazy.ServiceURL, modelID, lazy.ModelsDir, lazy.OnChunkReceived, lazy.MaxBytes)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mgr != nil {
		_ = e.mgr.Stop()
	}
	cfg := engine.Config{
		BinaryPath: lazy.EngineBin,
		ModelPath:  path,
		Port:       lazy.Port,
		CtxSize:    lazy.CtxSize,
		Threads:    lazy.Threads,
		CPUPercent: lazy.CPUPercent,
		MemBytes:   lazy.MemBytes,
	}
	if len(rpcBackends) > 0 {
		// Joint compute: offload layers to rpc-server workers.
		// --n-gpu-layers is required; without it all layers stay local.
		cfg.ExtraArgs = []string{"--rpc", strings.Join(rpcBackends, ","), "--n-gpu-layers", fmt.Sprint(ngl)}
	}
	mgr := engine.New(cfg)
	if err := mgr.Start(ctx); err != nil {
		return nil, fmt.Errorf("engine start: %w", err)
	}
	e.mgr = mgr
	e.rpcBackends = rpcBackends
	e.rpcNgl = ngl
	e.modelID = modelID
	if lazy.OnModelReady != nil {
		go lazy.OnModelReady(modelID, path)
	}
	return mgr, nil
}
