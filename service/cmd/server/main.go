// Command server runs the Distributed AI central service (M1).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"distributed-ai/engine"
	"distributed-ai/service/internal/api"
	"distributed-ai/service/internal/credit"
	"distributed-ai/service/internal/p2pnet"
	"distributed-ai/service/internal/quest"
	"distributed-ai/service/internal/registry"
	"distributed-ai/service/internal/replica"
)

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	reg := registry.New()
	srv := api.New(reg)

	// Model manifests: MODELS_DIR (default ../models relative to service/).
	// Only size+SHA-256-verified files become ACTIVE.
	modelsDir := os.Getenv("MODELS_DIR")
	if modelsDir == "" {
		modelsDir = "../models"
	}
	if err := reg.LoadModels(modelsDir, modelsDir); err != nil {
		log.Fatalf("load models: %v", err)
	}

	// Optional local engine: ENGINE_BIN + MODEL_PATH must both be set.
	// Without them the service runs in control-plane-only mode and
	// /v1/chat/completions returns 503 engine_not_ready.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var mgr *engine.Manager
	if bin, model := os.Getenv("ENGINE_BIN"), os.Getenv("MODEL_PATH"); bin != "" && model != "" {
		port := 18080
		if p := os.Getenv("ENGINE_PORT"); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
		cfg := engine.Config{BinaryPath: bin, ModelPath: model, Port: port}
		// ENGINE_CTX_SIZE: Oracle 1GB 박스 같은 저메모리 환경을 위한 조절.
		// 기본값이면 llama-server 기본값 사용.
		if c := os.Getenv("ENGINE_CTX_SIZE"); c != "" {
			if n, err := strconv.Atoi(c); err == nil && n > 0 {
				cfg.CtxSize = n
			}
		}
		mgr = engine.New(cfg)
		log.Printf("starting inference engine: %s", bin)
		if err := mgr.Start(ctx); err != nil {
			log.Fatalf("engine failed: %v", err)
		}
		defer mgr.Stop()
		srv.WithEngine(mgr)
		log.Printf("engine ready at %s", mgr.BaseURL())
	} else {
		log.Printf("no ENGINE_BIN/MODEL_PATH — running without local inference engine")
	}

	log.Printf("distributed-ai service listening on %s", addr)

	// Service libp2p host: used for dial-back verification (M3b-1) and,
	// later, remote job dispatch (M3b-2).
	p2phost, err := p2pnet.New()
	if err != nil {
		log.Fatalf("p2p host: %v", err)
	}
	defer p2phost.Close()
	log.Printf("p2p host: %s", p2pnet.PeerAddrString(p2phost))
	srv.WithP2PHost(p2phost).WithLedger(credit.NewMemoryLedger())

	// TEST_FUND="account:credits[:apikey]" — alpha testing only. Funds an
	// account at startup and optionally sets a known API key (the ledger
	// and keys are in-memory; this is the only way to fund without earning
	// via storage/transfer).
	if tf := os.Getenv("TEST_FUND"); tf != "" {
		parts := strings.SplitN(tf, ":", 3)
		if len(parts) >= 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				srv.Ledger().Earn(parts[0], "test-fund", "test", int64(n))
				if len(parts) == 3 {
					srv.APIKeys().IssueFor(parts[0], parts[2])
				}
				log.Printf("TEST_FUND: %s += %d", parts[0], n)
			}
		}
	}
	// Founder 1% share: DISABLED by default. Enabled only with explicit
	// user approval via FOUNDER_SHARE_ENABLED=1.
	if founder := os.Getenv("FOUNDER_ACCOUNT"); founder != "" {
		enabled := os.Getenv("FOUNDER_SHARE_ENABLED") == "1"
		srv.SetFounder(founder, enabled)
		if enabled {
			log.Printf("founder account: %s (1%% of earnings — explicitly enabled)", founder)
		} else {
			log.Printf("founder account: %s (1%% share disabled — awaiting approval)", founder)
		}
	}

	// Dial-back verification: prove which nodes are directly reachable.
	// Only dial-back can set DirectOK — nodes never self-report it.
	// New registrations verify immediately; a periodic sweep catches nodes
	// whose network changed since.
	verifyNode := func(id string) {
		n := reg.Get(id)
		if n == nil {
			return
		}
		if ok, rtt := p2pnet.DialBack(ctx, p2phost, n.ID, n.Addrs, n.SourceIP); ok {
			reg.SetDirectOK(n.ID, true, rtt)
			log.Printf("dial-back: node %s directly reachable (rtt %dms)", n.ID, rtt.Milliseconds())
		}
	}
	reg.OnVerify(verifyNode)
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, n := range reg.UnverifiedNodes() {
					verifyNode(n.ID)
				}
			}
		}
	}()

	// Replica reconciler: every minute, converge toward desired holder
	// counts — healing when a holder left, scaling when demand is hot.
	// Assignments ride the nodes' heartbeats (NAT-friendly pull).
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, a := range replica.Plan(reg, srv.Metrics()) {
					reg.QueueReplication(a.NodeID, a.ModelID)
					log.Printf("replica: assigned %s → node %s", a.ModelID, a.NodeID)
				}
			}
		}
	}()

	// Model Quest watcher: logs when a ladder model unlocks (LOCKED→ELIGIBLE)
	// or activates, derived from live snapshots every minute.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		prev := map[string]string{}
		check := func() {
			for _, e := range quest.Evaluate(reg) {
				if prev[e.ModelID] == "" {
					prev[e.ModelID] = e.State
					continue
				}
				if prev[e.ModelID] != e.State {
					log.Printf("quest: %s %s → %s (progress %.0f%%)",
						e.ModelID, prev[e.ModelID], e.State, e.Progress*100)
					prev[e.ModelID] = e.State
				}
			}
		}
		check() // baseline at startup
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- http.ListenAndServe(addr, srv.Routes()) }()
	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-serveErr:
		// Bind failure etc: don't leave the engine orphaned.
		// (os.Exit skips defers, so stop it explicitly.)
		log.Printf("http server failed: %v", err)
		if mgr != nil {
			_ = mgr.Stop()
		}
		os.Exit(1)
	}
}
