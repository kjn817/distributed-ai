// Command node runs the Distributed AI Node Runtime.
//
// Merged from the original distributed-ai-node runtime: behavior preserved,
// structure split into packages. Execution strategy note: this node currently
// operates under FULL_MATERIALIZE (a full model file is assembled before
// llama.cpp loads it). That is the v1 execution strategy, not a permanent
// architectural invariant — see docs/manifest-spec.md.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"distributed-ai/engine"
	"distributed-ai/limits"
	"distributed-ai/node/internal/chunks"
	"distributed-ai/node/internal/executor"
	"distributed-ai/node/internal/hardware"
	"distributed-ai/node/internal/identity"
	"distributed-ai/node/internal/materialize"
	"distributed-ai/node/internal/p2p"
	"distributed-ai/node/internal/protocol"
	"distributed-ai/node/internal/reporter"
	"distributed-ai/node/internal/rpcworker"
	jobprotocol "distributed-ai/protocol"
)

// version is overridden at release build time:
//   go build -ldflags "-X main.version=v0.9-alpha" ./node/cmd/node
var version = "dev"

func printLocalInfo(info protocol.ResourceInfo) {
	fmt.Println("================================")
	fmt.Println(" Distributed AI Node v0.8")
	fmt.Println("================================")
	fmt.Println("Peer ID:", info.PeerID)
	fmt.Println("OS:", info.OS)
	fmt.Println("Architecture:", info.Architecture)
	fmt.Println("CPU:", info.CPUModel)
	fmt.Println("Physical cores:", info.PhysicalCores)
	fmt.Println("Logical cores:", info.LogicalCores)
	fmt.Printf("RAM: %.1f GB\n", info.RAMTotalGB)
	fmt.Println("GPU:", info.GPUModel)
	if info.GPUVendor != "" {
		fmt.Println("GPU vendor:", info.GPUVendor)
	}
	if info.GPUCores > 0 {
		fmt.Println("GPU cores:", info.GPUCores)
	}
	if info.GPUDriver != "" {
		fmt.Println("GPU driver:", info.GPUDriver)
	}
	if info.MetalSupport != "" {
		fmt.Println("Metal:", info.MetalSupport)
	}
	fmt.Printf("Storage total: %.1f GB\n", info.StorageTotalGB)
	fmt.Printf("Storage used: %.1f GB\n", info.StorageUsedGB)
	fmt.Printf("Storage free: %.1f GB\n", info.StorageFreeGB)
	fmt.Printf(
		"Sharing: CPU %d%% / GPU %d%% / RAM %d%% / Storage %d%%\n",
		info.CPUShare, info.GPUShare, info.RAMShare, info.StorageShare,
	)
	// Honest platform enforcement status.
	if ps := limits.PlatformSupport(); ps != nil {
		if desc, ok := ps[runtime.GOOS]; ok {
			fmt.Printf("OS enforcement [%s]: %s\n", runtime.GOOS, desc)
		}
	}
	if info.GPUShare == 100 {
		fmt.Printf("WARNING: GPU sharing is WHOLE-DEVICE (no fractional cap possible). " +
			"The engine may use the full GPU.\n")
	}
}

func main() {
	connectAddr := flag.String("connect", "", "peer multiaddress to connect to")
	apiURL := flag.String("api", "", "service base URL to register/heartbeat to (e.g. http://oracle:8080)")
	cpuShare := flag.Int("cpu", 10, "CPU share percent")
	gpuShare := flag.Int("gpu", 0, "GPU share percent: 0=disabled, 100=full GPU (explicit consent). Partial values (1-99) are NOT supported — no vendor API can enforce fractional GPU compute.")
	ramShare := flag.Int("ram", 10, "RAM share percent")
	storageShare := flag.Int("storage", 10, "storage share percent")
	p2pPort := flag.Int("p2p-port", 4001, "libp2p listen port")
	rpcPort := flag.Int("rpc-port", 0, "run as RPC worker on this port (0 = disabled); needs RPC_BIN and a model")
	rpcAnnounce := flag.String("rpc-announce", "", "address to advertise for RPC (default: auto-detected outbound IP)")
	claimOnly := flag.Bool("claim", false, "prove peer-ID ownership to the service and print an API key for the node's earnings account, then exit")

	// Fast --help/--version exit with status 0, before flag.Parse().
	// CI probes `<binary> --help` as an executability check: the process
	// must terminate promptly and must never fall through to node startup
	// (Go's flag package would otherwise exit 2 on --help).
	for _, a := range os.Args[1:] {
		switch a {
		case "-h", "--help":
			fmt.Println("dain-node — Distributed AI Node Runtime")
			fmt.Println()
			fmt.Println("Usage: dain-node [options]")
			fmt.Println()
			fmt.Println("Options:")
			flag.PrintDefaults()
			os.Exit(0)
		case "--version":
			fmt.Println("dain-node " + version)
			os.Exit(0)
		}
	}
	flag.Parse()

	// Identity first: everything else keys off the peer ID.
	priv, err := identity.LoadOrCreate()
	if err != nil {
		log.Fatal(err)
	}
	peerID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		log.Fatal(err)
	}

	if *claimOnly {
		if *apiURL == "" {
			log.Fatal("--claim requires --api")
		}
		key, err := claimAPIKey(*apiURL, peerID.String(), priv)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("API key for", peerID.String()+":")
		fmt.Println(key)
		fmt.Println("(store it now — the service never shows it again)")
		return
	}

	h, err := p2p.NewWithPort(priv, *p2pPort)
	if err != nil {
		panic(err)
	}
	defer h.Close()

	hostname, _ := os.Hostname()
	cpuModel, physicalCores, logicalCores, ramGB := hardware.DetectHardware()
	gpuModel, gpuVendor, gpuCores, gpuDriver, metalSupport := hardware.DetectGPU()
	storageTotalGB, storageUsedGB, storageFreeGB := hardware.DetectStorage()

	localInfo := protocol.ResourceInfo{
		NodeName:       hostname,
		PeerID:         h.ID().String(),
		OS:             runtime.GOOS,
		Architecture:   runtime.GOARCH,
		CPUModel:       cpuModel,
		PhysicalCores:  physicalCores,
		LogicalCores:   logicalCores,
		RAMTotalGB:     ramGB,
		GPUModel:       gpuModel,
		GPUVendor:      gpuVendor,
		GPUCores:       gpuCores,
		GPUDriver:      gpuDriver,
		MetalSupport:   metalSupport,
		StorageTotalGB: storageTotalGB,
		StorageUsedGB:  storageUsedGB,
		StorageFreeGB:  storageFreeGB,
		CPUShare:       *cpuShare,
		GPUShare:       *gpuShare,
		RAMShare:       *ramShare,
		StorageShare:   *storageShare,
	}
	// SSD share budget for the models directory (real enforcement).
	_, _, storageFreeGB = hardware.DetectStorage()
	maxBytes := int64(float64(storageFreeGB) * float64(localInfo.StorageShare) / 100 * 1e9)

	printLocalInfo(localInfo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Model library: modelID -> local file path. Pre-placed entries first;
	// materialized models are added as they arrive.
	var libMu sync.Mutex
	modelFiles := map[string]string{}
	modelsList := func() []string {
		libMu.Lock()
		defer libMu.Unlock()
		out := make([]string, 0, len(modelFiles))
		for id := range modelFiles {
			out = append(out, id)
		}
		return out
	}

	addrs := make([]string, 0, len(h.Addrs()))
	for _, a := range h.Addrs() {
		addrs = append(addrs, a.String())
	}

	// Chunk serving: always on — a node holding a model file serves its
	// 64MB chunks to peers (the P2P distribution mechanism).
	chunkSrv := chunks.New(
		func(modelID string) (string, bool) {
			libMu.Lock()
			defer libMu.Unlock()
			p, ok := modelFiles[modelID]
			return p, ok
		})
	h.SetStreamHandler(jobprotocol.ChunkProtocol, chunkSrv.HandleStream)

	// Inference executor (optional).
	// - Pre-placed: ENGINE_BIN+MODEL_PATH+MODEL_ID → engine starts now.
	// - Lazy: ENGINE_BIN+MODEL_ID (+ --api) → first job materializes the
	//   model over P2P chunk transfer, then serves it.
	// - Otherwise the node is share-and-report only.
	//
	// recvByPeer tracks receiver-confirmed chunk bytes per source peer
	// (drained by each heartbeat). Transfer credit is paid on this —
	// the downloader's word — not on the sender's self-report.
	var recvMu sync.Mutex
	recvByPeer := make(map[string]int64)
	// rpcAddr is set when this node runs as an RPC worker.
	var rpcMu sync.Mutex
	rpcAddr := ""
	getRpcAddr := func() string {
		rpcMu.Lock()
		defer rpcMu.Unlock()
		return rpcAddr
	}
	// engineMgr tracks the live inference engine (for OS cap status).
	var engineMu sync.Mutex
	var engineMgr *engine.Manager
	onChunk := func(peerID string, n int64) {
		recvMu.Lock()
		recvByPeer[peerID] += n
		recvMu.Unlock()
	}
	drainReceived := func() map[string]int64 {
		recvMu.Lock()
		defer recvMu.Unlock()
		out := recvByPeer
		recvByPeer = make(map[string]int64)
		return out
	}
	if bin := os.Getenv("ENGINE_BIN"); bin != "" {
		modelID := os.Getenv("MODEL_ID")
		if modelID == "" {
			log.Fatal("MODEL_ID must be set alongside ENGINE_BIN")
		}
		port := 18082
		if p := os.Getenv("ENGINE_PORT"); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
		ctxSize := 0
		if c := os.Getenv("ENGINE_CTX_SIZE"); c != "" {
			if n, err := strconv.Atoi(c); err == nil && n > 0 {
				ctxSize = n
			}
		}
		// recvByPeer moved to function scope (shared by executor + reporter).
		// CPU share enforcement: the engine never uses more threads than the
		// user shared, no matter how many cores the box has.
		// OS caps: cgroup v2 on Linux (real enforcement); elsewhere the
		// status is reported honestly as unenforced.
		threads := reporter.SharedCores(localInfo.LogicalCores, localInfo.CPUShare)
		memBytes := int64(float64(ramGB) * float64(localInfo.RAMShare) / 100 * 1e9)
		engineCfg := func(modelPath string) engine.Config {
			return engine.Config{
				BinaryPath: bin, ModelPath: modelPath, Port: port,
				CtxSize: ctxSize, Threads: threads,
				CPUPercent: localInfo.CPUShare, MemBytes: memBytes,
			}
		}
		if modelPath := os.Getenv("MODEL_PATH"); modelPath != "" {
			mgr := engine.New(engineCfg(modelPath))
			fmt.Println("Starting inference engine:", bin)
			if err := mgr.Start(ctx); err != nil {
				log.Fatalf("engine failed: %v", err)
			}
			engineMu.Lock()
			engineMgr = mgr
			engineMu.Unlock()
			defer mgr.Stop()
			// RPC worker: jointly compute with a coordinator.
			if *rpcPort > 0 {
				rpcBin := os.Getenv("RPC_BIN")
				rw := rpcworker.New(rpcBin, "0.0.0.0", *rpcPort)
				fmt.Println("Starting RPC worker:", rpcBin, "port", *rpcPort)
				if err := rw.Start(ctx); err != nil {
					log.Fatalf("rpc worker failed: %v", err)
				}
				defer rw.Stop()
				// Report the dialable address (service gives it to coordinators).
				// NAT workers need manual port-forwarding (documented limitation).
				announce := *rpcAnnounce
				if announce == "" {
					if ip := outboundIP(); ip != "" {
						announce = ip
					} else {
						announce = "127.0.0.1"
					}
				}
				rpcMu.Lock()
				rpcAddr = fmt.Sprintf("%s:%d", announce, *rpcPort)
				rpcMu.Unlock()
				fmt.Println("RPC worker ready")
			}
			libMu.Lock()
			modelFiles[modelID] = modelPath
			libMu.Unlock()
			h.SetStreamHandler(jobprotocol.JobProtocol, executor.New(mgr, modelID, engineCfg(modelPath)).HandleStream)
			fmt.Println("Job executor ready, serving model:", modelID)
		} else if *apiURL != "" {
			home, _ := os.UserHomeDir()
			modelsDir := filepath.Join(home, ".distributed-ai-node", "models")
			ex := executor.NewLazy(executor.LazyConfig{
				EngineBin:       bin,
				Port:            port,
				CtxSize:         ctxSize,
				Threads:         threads,
				CPUPercent:      localInfo.CPUShare,
				MemBytes:        memBytes,
				MaxBytes:        maxBytes,
				ServiceURL:      *apiURL,
				ModelsDir:       modelsDir,
				OnChunkReceived: onChunk,
				Host:            h,
				OnModelReady: func(mid, path string) {
					libMu.Lock()
					modelFiles[mid] = path
					libMu.Unlock()
					fmt.Println("Model materialized:", mid, "->", path)
					// Re-register so the service lists us as a holder.
					_ = reporter.Register(context.Background(), *apiURL, localInfo, addrs, modelsList(), getRpcAddr())
				},
			})
			h.SetStreamHandler(jobprotocol.JobProtocol, ex.HandleStream)
			fmt.Println("Job executor ready (lazy), will materialize model:", modelID)
		} else {
			fmt.Println("ENGINE_BIN set but no MODEL_PATH and no --api: executor disabled")
		}
	}

	// Report to the central service so /network/status shows real numbers.
	// Runs alongside the libp2p listener; independent of -connect.
	if *apiURL != "" {
		home, _ := os.UserHomeDir()
		modelsDir := filepath.Join(home, ".distributed-ai-node", "models")
		// replicate fetches an assigned model in the background (holder,
		// not compute: no engine is started for it).
		replicate := func(modelID string) {
			path, err := materialize.Materialize(context.Background(), h, *apiURL, modelID, modelsDir, onChunk, maxBytes)
			if err != nil {
				fmt.Println("Replicate:", modelID, "failed:", err)
				return
			}
			libMu.Lock()
			modelFiles[modelID] = path
			libMu.Unlock()
			fmt.Println("Replicated:", modelID, "->", path)
			_ = reporter.Register(context.Background(), *apiURL, localInfo, addrs, modelsList(), getRpcAddr())
		}
		go reporter.Run(context.Background(), reporter.Config{
			APIURL:     *apiURL,
			Info:       localInfo,
			Addrs:      addrs,
			Models:     modelsList,
			ChunkBytes: chunkSrv.BytesServed,
			ChunkReceived: func() []reporter.ReceivedFrom {
				out := []reporter.ReceivedFrom{}
				for peerID, n := range drainReceived() {
					out = append(out, reporter.ReceivedFrom{PeerID: peerID, Bytes: n})
				}
				return out
			},
			RpcAddr: func() string { return getRpcAddr() },
			Limits: func() reporter.LimitsReport {
				engineMu.Lock()
				mgr := engineMgr
				engineMu.Unlock()
				if mgr == nil {
					return reporter.LimitsReport{}
				}
				st := mgr.LimitsStatus()
				return reporter.LimitsReport{Enforced: st.Enforced, Mechanism: st.Mechanism, Reason: st.Reason}
			},
			Interval: 30 * time.Second,
			OnErr:    func(err error) { fmt.Println("Reporter:", err) },
			OnAssign: func(modelIDs []string) {
				for _, mid := range modelIDs {
					go replicate(mid)
				}
			},
		})
		fmt.Println("Reporting to service:", *apiURL)
	}

	if *connectAddr != "" {
		fmt.Println("Connecting to:", *connectAddr)
		if err := p2p.ConnectAndSend(context.Background(), h, *connectAddr, localInfo); err != nil {
			fmt.Println("Connection failed:", err)
		} else {
			fmt.Println("✅ P2P CONNECTED and resource info sent")
		}
	}

	fmt.Println()
	fmt.Println("Node is running. Press Ctrl+C to stop.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("\nNode stopped.")
}

// claimAPIKey proves peer-ID ownership to the service via challenge-response
// and returns an API key for the node's earnings account.
func claimAPIKey(apiURL, peerID string, priv crypto.PrivKey) (string, error) {
	post := func(path string, body any) (map[string]any, error) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(apiURL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("service: %v", out["error"])
		}
		return out, nil
	}
	chResp, err := post("/v1/accounts/claim", map[string]string{"peer_id": peerID})
	if err != nil {
		return "", err
	}
	challenge, _ := chResp["challenge"].(string)
	chBytes, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil {
		return "", err
	}
	sig, err := priv.Sign(chBytes)
	if err != nil {
		return "", err
	}
	verResp, err := post("/v1/accounts/claim/verify", map[string]string{
		"peer_id":   peerID,
		"challenge": challenge,
		"signature": base64.RawURLEncoding.EncodeToString(sig),
	})
	if err != nil {
		return "", err
	}
	key, _ := verResp["api_key"].(string)
	if key == "" {
		return "", fmt.Errorf("no api_key in response")
	}
	return key, nil
}

// outboundIP returns the node's preferred outward-facing IPv4 (for RPC
// dial-back by coordinators). Empty when it can't be determined.
func outboundIP() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	if addr, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}
