package placement

// Measurement interface for Adaptive Compute Admission.
//
// DecideAdmission consumes AdmissionInputs; this file defines where real
// inputs come from. A Measurement is one observed run (or probe) with
// explicit provenance: every numeric field is a Quantity, so a value
// that was not measured stays Measured=false end-to-end and the
// admission decision degrades to insufficient_data instead of guessing.
//
// Timing scopes are those of MeasurementScopes: compute times are pure
// compute (no network, no sync waits), network time is priced once via
// RTT × rounds + derived transfer, overhead is the residual only.
// Collectors must respect the scopes when filling fields — a collector
// that records end-to-end wall time belongs in WallMs, not ComputeMs.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Roles a measurement can describe.
const (
	RoleSingle      = "single"
	RoleCoordinator = "coordinator"
	RoleWorker      = "worker"
)

// Error types for failed runs (Success=false). Empty means success.
const (
	ErrTypeTimeout        = "timeout"
	ErrTypeRPCUnreachable = "rpc_unreachable"
	ErrTypeEngine         = "engine_error"
	ErrTypeOOM            = "oom"
	ErrTypeUnknown        = "unknown"
)

// Measurement is one observed execution or probe result.
type Measurement struct {
	ModelID string `json:"model_id"`
	// ModelWeightsHash is the SHA-256 (hex) of the actual weights file
	// the measurement ran against, so numbers are tied to exact weights.
	ModelWeightsHash string `json:"model_weights_hash,omitempty"`
	NodeID           string `json:"node_id"`
	Role             string `json:"role"` // single | coordinator | worker

	// WallMs: end-to-end wall time of the run on this node.
	WallMs Quantity `json:"wall_ms"`
	// PrefillMs / DecodeMs: engine-reported phase timings.
	PrefillMs Quantity `json:"prefill_ms"`
	DecodeMs  Quantity `json:"decode_ms"`
	// ComputeMs: pure compute attributed to this node in a joint run
	// (scope: excludes RPC transfer and synchronization waits).
	ComputeMs       Quantity `json:"compute_ms"`
	PromptTokens    int      `json:"prompt_tokens,omitempty"`
	GenTokens       int      `json:"gen_tokens,omitempty"`
	TokensPerSecond Quantity `json:"tokens_per_second"`

	// NetworkPath identifies the coordinator↔worker path the network
	// quantities were measured on (e.g. "coord-A->worker-B"). The
	// builder only combines network quantities whose path matches.
	NetworkPath   string   `json:"network_path,omitempty"`
	RTTMs         Quantity `json:"rtt_ms"`
	BandwidthMbps Quantity `json:"bandwidth_mbps"`
	// RTTRounds: protocol-known round trips for the joint run.
	RTTRounds int `json:"rtt_rounds,omitempty"`
	// RPCConnected: this node verified its RPC connection for the run.
	RPCConnected bool `json:"rpc_connected"`
	// TensorBytes: actual bytes exchanged with the peer for this run.
	TensorBytes Quantity `json:"tensor_bytes"`
	// OverheadMs: measured residual overhead (see scopes).
	OverheadMs Quantity `json:"overhead_ms"`

	Success   bool      `json:"success"`
	ErrorType string    `json:"error_type,omitempty"` // "" on success
	Source    string    `json:"source"`               // e.g. "llama-server timings"
	At        time.Time `json:"at"`
}

// ProofRecord is recorded evidence that joint execution actually
// offloaded work for a model (both-sides compute evidence, e.g. a
// Phase B proof). Timing measurements can never create one: production
// verification requires this explicit record with an evidence ref.
type ProofRecord struct {
	ModelID     string    `json:"model_id"`
	EvidenceRef string    `json:"evidence_ref"` // e.g. document/run ID
	At          time.Time `json:"at"`
}

// MeasurementStore is where collectors put measurements and where the
// admission builder reads them.
type MeasurementStore interface {
	Record(m Measurement)
	// ForModel returns recorded measurements for a model, oldest first.
	ForModel(modelID string) []Measurement
}

// ProofSource supplies joint-execution proof records.
type ProofSource interface {
	ProofFor(modelID string) (ProofRecord, bool)
}

// MemoryMeasurementStore is a bounded in-memory MeasurementStore and
// ProofSource. It is the reference implementation for tests and for the
// shadow integration; a durable store can replace it later.
type MemoryMeasurementStore struct {
	mu       sync.Mutex
	capacity int
	byModel  map[string][]Measurement
	proofs   map[string]ProofRecord
}

// NewMemoryMeasurementStore creates a store keeping at most capacity
// measurements per model (default 256).
func NewMemoryMeasurementStore(capacity int) *MemoryMeasurementStore {
	if capacity <= 0 {
		capacity = 256
	}
	return &MemoryMeasurementStore{
		capacity: capacity,
		byModel:  map[string][]Measurement{},
		proofs:   map[string]ProofRecord{},
	}
}

// Record appends a measurement (evicting the oldest beyond capacity).
func (s *MemoryMeasurementStore) Record(m Measurement) {
	if m.At.IsZero() {
		m.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lst := append(s.byModel[m.ModelID], m)
	if len(lst) > s.capacity {
		lst = append([]Measurement(nil), lst[len(lst)-s.capacity:]...)
	}
	s.byModel[m.ModelID] = lst
}

// ForModel implements MeasurementStore.
func (s *MemoryMeasurementStore) ForModel(modelID string) []Measurement {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Measurement(nil), s.byModel[modelID]...)
}

// RecordProof stores a joint-execution proof record for a model.
func (s *MemoryMeasurementStore) RecordProof(p ProofRecord) {
	if p.At.IsZero() {
		p.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proofs[p.ModelID] = p
}

// ProofFor implements ProofSource.
func (s *MemoryMeasurementStore) ProofFor(modelID string) (ProofRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proofs[modelID]
	return p, ok
}

// HashWeightsFile returns the SHA-256 hex digest of a weights file,
// streamed (the file is never loaded whole).
func HashWeightsFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// successful returns the successful measurements of one role for a node
// (nodeID "" = any node).
func successful(ms []Measurement, role, nodeID string) []Measurement {
	var out []Measurement
	for _, m := range ms {
		if m.Role != role || !m.Success {
			continue
		}
		if nodeID != "" && m.NodeID != nodeID {
			continue
		}
		out = append(out, m)
	}
	return out
}

// BuildAdmissionInputs assembles admission inputs from the measurement
// store. It is meant to be layered over the registry-derived inputs via
// MergeRegistryGates (which contributes model readiness and, as a
// fallback, the dial-back RTT). Anything the store cannot supply with
// a real measurement stays unmeasured.
//
// Combination rules:
//   - best_single_ms: the fastest successful single-node run — WallMs
//     when measured, else PrefillMs+DecodeMs when both are measured.
//   - coordinator/worker compute: successful records for the assigned
//     nodes. Multi-worker cohorts are not aggregated (v1 models the
//     single-worker case; anything else leaves worker_ms unmeasured).
//   - network quantities come from the coordinator's record, and are
//     accepted only when a worker record shares the same NetworkPath.
//   - rpc_reachable requires RPCConnected on BOTH the coordinator and
//     the worker record.
//   - joint_execution_proven comes only from a ProofRecord.
func BuildAdmissionInputs(store MeasurementStore, proofs ProofSource, req Request, a *Assignment) AdmissionInputs {
	in := AdmissionInputs{RequestID: "", ModelID: req.ModelID}
	if store == nil {
		in.Notes = append(in.Notes, "no measurement store configured")
		return in
	}
	ms := store.ForModel(req.ModelID)
	if len(ms) == 0 {
		in.Notes = append(in.Notes, "no measurements recorded for this model yet")
		return in
	}

	// Best single-node baseline.
	best := 0.0
	bestSrc := ""
	for _, m := range successful(ms, RoleSingle, "") {
		var total float64
		switch {
		case m.WallMs.Measured:
			total = m.WallMs.Value
		case m.PrefillMs.Measured && m.DecodeMs.Measured:
			total = m.PrefillMs.Value + m.DecodeMs.Value
		default:
			continue
		}
		if best == 0 || total < best {
			best, bestSrc = total, m.Source
		}
		if m.TokensPerSecond.Measured {
			in.Notes = append(in.Notes, fmt.Sprintf("single-node measurement: %.1f tok/s, %d prompt + %d gen tokens (node %s)", m.TokensPerSecond.Value, m.PromptTokens, m.GenTokens, m.NodeID))
		}
		if m.ModelWeightsHash != "" {
			in.Notes = append(in.Notes, "weights sha256: "+m.ModelWeightsHash)
		}
	}
	if best > 0 {
		in.BestSingleMs = MeasureQS(best, bestSrc)
	}

	if a == nil || a.NodeID == "" {
		return in
	}
	coords := successful(ms, RoleCoordinator, a.NodeID)
	if len(coords) == 0 {
		in.Notes = append(in.Notes, "no successful coordinator measurement for the assigned node")
		return in
	}
	coord := coords[len(coords)-1] // latest
	if coord.ComputeMs.Measured {
		in.CoordinatorMs = coord.ComputeMs
	}

	if a.Kind != "distributed" || len(a.Cohort) != 1 {
		if a.Kind == "distributed" && len(a.Cohort) != 1 {
			in.Notes = append(in.Notes, "multi-worker cohorts are not aggregated by the v1 measurement builder; worker_ms left unmeasured")
		}
		return in
	}
	workers := successful(ms, RoleWorker, a.Cohort[0].NodeID)
	if len(workers) == 0 {
		in.Notes = append(in.Notes, "no successful worker measurement for the cohort worker")
		return in
	}
	worker := workers[len(workers)-1]
	if worker.ComputeMs.Measured {
		in.WorkerMs = worker.ComputeMs
	}

	// Network quantities: same-path requirement.
	if coord.NetworkPath != "" && worker.NetworkPath == coord.NetworkPath {
		if coord.RTTMs.Measured {
			in.RTTMs = coord.RTTMs
		}
		if coord.BandwidthMbps.Measured {
			in.BandwidthMbps = coord.BandwidthMbps
		}
		if coord.TensorBytes.Measured {
			in.PayloadBytes = coord.TensorBytes
		}
		if coord.OverheadMs.Measured {
			in.OverheadMs = coord.OverheadMs
		}
		if coord.RTTRounds > 0 {
			in.RTTRounds = coord.RTTRounds
			in.RTTRoundsKnown = true
		}
	} else {
		in.Notes = append(in.Notes, "network quantities rejected: coordinator and worker measurements are not from the same path")
	}

	// Gates from measurements (model readiness is merged by the caller
	// from the registry builder; see MergeRegistryGates).
	in.Gates.RPCReachable = coord.RPCConnected && worker.RPCConnected
	if proofs != nil {
		if p, ok := proofs.ProofFor(req.ModelID); ok && p.EvidenceRef != "" {
			in.Gates.JointExecutionProven = true
			in.Gates.ProofEvidence = p.EvidenceRef
		}
	}
	return in
}

// MergeRegistryGates copies the registry-derived gates that
// measurements do not cover (model readiness) into inputs built from
// measurements, without overwriting measurement-derived network inputs.
func MergeRegistryGates(dst *AdmissionInputs, registryInputs AdmissionInputs) {
	dst.Gates.ModelReady = registryInputs.Gates.ModelReady
	dst.Notes = append(dst.Notes, "model_ready evaluated from the registry for all assignment participants")
	if !dst.RTTMs.Measured && registryInputs.RTTMs.Measured {
		dst.RTTMs = registryInputs.RTTMs
		dst.Notes = append(dst.Notes, registryInputs.Notes...)
	}
}
