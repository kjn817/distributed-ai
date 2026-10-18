// Package registry holds the in-memory Node and Model registries.
// v1: in-memory with mutex. Swap to PostgreSQL when node count or
// durability needs it — the interface stays the same.
package registry

import (
	"path/filepath"
	"sync"
	"time"

	"distributed-ai/service/internal/model"
)

// Node is a single participant in the network.
type Node struct {
	ID             string   `json:"id"`
	OS             string   `json:"os"`
	CPUCores       int      `json:"cpu_cores"`
	CPUModel       string   `json:"cpu_model"`
	GPUModel       string   `json:"gpu_model"`
	VRAMBytes      int64    `json:"vram_bytes"`
	RAMBytes       int64    `json:"ram_bytes"`
	SSDSharedBytes int64    `json:"ssd_shared_bytes"`
	DirectOK       bool     `json:"direct_ok"` // set only by service dial-back, never self-reported
	RTTMs          int64    `json:"rtt_ms"`    // measured dial-back RTT; 0 = not yet measured
	InFlight       int      `json:"in_flight"` // requests currently dispatched here
	Addrs          []string `json:"addrs,omitempty"`
	SourceIP       string   `json:"-"`                // HTTP source IP at registration (dial-back candidate)
	Models         []string `json:"models,omitempty"` // model IDs this node can serve
	// LimitsEnforced/Mechanism/Reason: OS cap status from the node's heartbeat.
	LimitsEnforced  bool   `json:"limits_enforced"`
	LimitsMechanism string `json:"limits_mechanism,omitempty"`
	LimitsReason    string `json:"limits_reason,omitempty"`
	// RpcAddr is "host:port" of the node's rpc-server (empty = not a worker).
	// Workers jointly compute one request with a coordinator via llama.cpp RPC.
	RpcAddr       string    `json:"rpc_addr,omitempty"`
	LastHeartbeat time.Time `json:"-"`
}

// Model is a model known to the network (manifest summary).
type Model struct {
	ModelID  string `json:"model_id"`
	Version  int    `json:"version"`
	Quant    string `json:"quant"`
	SizeByte int64  `json:"size_bytes"`
	Status   string `json:"status"` // LOCKED, ELIGIBLE, PREPARING, REPLICATING, READY, ACTIVE, DEGRADED
}

const heartbeatTimeout = 90 * time.Second

// Registry is the in-memory store.
type Registry struct {
	mu        sync.RWMutex
	nodes     map[string]*Node
	models    map[string]*Model
	manifests map[string]*model.Manifest // full manifests, by model ID
	pending   map[string][]string        // node ID → model IDs queued for replication
	// xferPending accumulates receiver-confirmed bytes per sender until a
	// whole GB is creditable.
	xferPending map[string]int64
	onVerify    func(id string) // called (in background) when a node registers
}

// New creates an empty registry. Models are loaded via LoadModels —
// no mock seed data: without manifests the model list is honestly empty.
func New() *Registry {
	return &Registry{
		nodes:       make(map[string]*Node),
		models:      make(map[string]*Model),
		manifests:   make(map[string]*model.Manifest),
		pending:     make(map[string][]string),
		xferPending: make(map[string]int64),
	}
}

// OnVerify registers a callback invoked in a new goroutine whenever a node
// registers, so dial-back verification starts immediately instead of waiting
// for the periodic sweep.
func (r *Registry) OnVerify(f func(id string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onVerify = f
}

// LoadModels registers every manifest in manifestDir. A model becomes ACTIVE
// only if its file in fileDir passes size+SHA-256 verification; otherwise it
// stays PREPARING (known but not materialized).
func (r *Registry) LoadModels(manifestDir, fileDir string) error {
	manifests, err := model.LoadDir(manifestDir)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range manifests {
		status := "PREPARING"
		if err := model.Verify(m, filepath.Join(fileDir, m.Source.File)); err == nil {
			status = "ACTIVE"
		}
		r.models[m.ModelID] = &Model{
			ModelID:  m.ModelID,
			Version:  m.Version,
			Quant:    m.Quant,
			SizeByte: m.SizeBytes,
			Status:   status,
		}
		r.manifests[m.ModelID] = m
	}
	return nil
}

// Manifest returns the full manifest for a model, if known.
func (r *Registry) Manifest(id string) (*model.Manifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.manifests[id]
	return m, ok
}

// ModelHolders returns online nodes that serve the given model.
func (r *Registry) ModelHolders(modelID string) []*Node {
	all := r.OnlineNodes()
	var out []*Node
	for _, n := range all {
		for _, m := range n.Models {
			if m == modelID {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// Register adds or updates a node.
func (r *Registry) Register(n *Node) {
	r.mu.Lock()
	n.LastHeartbeat = time.Now()
	r.nodes[n.ID] = n
	cb := r.onVerify
	r.mu.Unlock()
	// Verify outside the lock: the callback dials back and calls SetDirectOK.
	if cb != nil {
		go cb(n.ID)
	}
}

// Heartbeat refreshes a node's liveness. Returns false if unknown.
func (r *Registry) Heartbeat(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return false
	}
	n.LastHeartbeat = time.Now()
	return true
}

// SetDirectOK records the dial-back verification result and the measured
// RTT. Only the service calls this — nodes never self-report reachability.
func (r *Registry) SetDirectOK(id string, ok bool, rtt time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, found := r.nodes[id]; found {
		n.DirectOK = ok
		if ok {
			n.RTTMs = rtt.Milliseconds()
		}
	}
}

// QueueReplication asks node id to fetch modelID on its next heartbeat.
// Idempotent: the same model is never queued twice for one node.
func (r *Registry) QueueReplication(nodeID, modelID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.pending[nodeID] {
		if m == modelID {
			return
		}
	}
	r.pending[nodeID] = append(r.pending[nodeID], modelID)
}

// TakeReplications returns and clears the queued replication assignments
// for node id (delivered via heartbeat).
func (r *Registry) TakeReplications(nodeID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.pending[nodeID]
	delete(r.pending, nodeID)
	return out
}

// AddInFlight adjusts the in-flight dispatch count for a node (called by
// the API handler around remote dispatch; placement reads it).
func (r *Registry) AddInFlight(id string, delta int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, found := r.nodes[id]; found {
		n.InFlight += delta
		if n.InFlight < 0 {
			n.InFlight = 0
		}
	}
}

// SetRpcAddr records the node's rpc-server address ("" = not a worker).
func (r *Registry) SetRpcAddr(id, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[id]; ok {
		n.RpcAddr = addr
	}
}

// SetLimits records the node's OS resource-cap enforcement status.
func (r *Registry) SetLimits(id string, enforced bool, mechanism, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[id]; ok {
		n.LimitsEnforced = enforced
		n.LimitsMechanism = mechanism
		n.LimitsReason = reason
	}
}

// AddTransferBytes records receiver-confirmed chunk bytes from a sender
// and returns whole GBs newly creditable (dust stays pending). Transfer
// credit is paid on the DOWNLOADER's report, not the sender's self-report,
// so a node cannot inflate its earnings by lying about bytes served.
func (r *Registry) AddTransferBytes(senderID string, bytes int64, quantum int64) int64 {
	if bytes <= 0 || quantum <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[senderID]; !ok {
		return 0 // unknown sender: don't mint credit
	}
	r.xferPending[senderID] += bytes
	pending := r.xferPending[senderID]
	pay := pending / quantum * quantum
	r.xferPending[senderID] = pending - pay
	return pay
}

// PendingCount reports how many replication assignments are queued.
func (r *Registry) PendingCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, ms := range r.pending {
		n += len(ms)
	}
	return n
}

// Get returns a copy of the node with the given ID, or nil.
func (r *Registry) Get(id string) *Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n, ok := r.nodes[id]; ok {
		cp := *n
		return &cp
	}
	return nil
}

// OnlineNodes returns nodes with a recent heartbeat.
func (r *Registry) OnlineNodes() []*Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cutoff := time.Now().Add(-heartbeatTimeout)
	out := make([]*Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		if n.LastHeartbeat.After(cutoff) {
			out = append(out, n)
		}
	}
	return out
}

// UnverifiedNodes returns online nodes whose direct reachability has not
// been proven yet (candidates for dial-back).
func (r *Registry) UnverifiedNodes() []*Node {
	all := r.OnlineNodes()
	out := make([]*Node, 0, len(all))
	for _, n := range all {
		if !n.DirectOK {
			out = append(out, n)
		}
	}
	return out
}

// ComputeNodes returns online nodes reachable directly (no relay).
// Defensive default: relayed nodes are storage-only, never compute.
func (r *Registry) ComputeNodes() []*Node {
	all := r.OnlineNodes()
	out := make([]*Node, 0, len(all))
	for _, n := range all {
		if !n.DirectOK {
			continue
		}
		// CPU 0% share = no inference work. The node may still serve as
		// a storage holder (replication), but never gets compute jobs.
		if n.CPUCores <= 0 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Models returns all known models.
func (r *Registry) Models() []*Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Model, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m)
	}
	return out
}

// Totals aggregates network resources for the dashboard.
func (r *Registry) Totals() map[string]any {
	nodes := r.OnlineNodes()
	var cpu, ram, ssd, vram int64
	gpuNodes := 0
	rpcWorkers := 0
	gpuModels := map[string]int{}
	for _, n := range nodes {
		cpu += int64(n.CPUCores)
		ram += n.RAMBytes
		ssd += n.SSDSharedBytes
		if n.GPUModel != "" {
			gpuNodes++
			vram += n.VRAMBytes
			gpuModels[n.GPUModel]++
		}
		if n.RpcAddr != "" {
			rpcWorkers++
		}
	}
	return map[string]any{
		"online_nodes": len(nodes),
		"cpu_cores":    cpu,
		"gpu_nodes":    gpuNodes,
		"gpu_models":   gpuModels,
		"vram_bytes":   vram,
		"ram_bytes":    ram,
		"ssd_bytes":    ssd,
		"rpc_workers":  rpcWorkers, // joint-compute workers (vs request-routing nodes)
	}
}
