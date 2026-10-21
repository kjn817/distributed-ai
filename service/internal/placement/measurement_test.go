package placement

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"distributed-ai/service/internal/registry"
)

func TestHashWeightsFileKnownVector(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.bin")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := HashWeightsFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// SHA-256("abc"), the standard test vector.
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if h != want {
		t.Fatalf("hash = %s, want %s", h, want)
	}
	if _, err := HashWeightsFile(p + ".missing"); err == nil {
		t.Fatal("missing file must error")
	}
}

// fullMeasurementSet returns a complete, successful measurement set for
// a coordinator+worker pair on one network path. Values are fixtures
// (hand-checkable), recorded through the store interface exactly as a
// collector would record real ones.
func fullMeasurementSet() *MemoryMeasurementStore {
	s := NewMemoryMeasurementStore(16)
	s.Record(Measurement{
		ModelID: "m1", ModelWeightsHash: "deadbeef", NodeID: "solo", Role: RoleSingle,
		WallMs: MeasureQS(225, "test engine timings"), PrefillMs: MeasureQ(50),
		DecodeMs: MeasureQ(175), TokensPerSecond: MeasureQ(42), PromptTokens: 10, GenTokens: 90,
		Success: true, Source: "test engine timings", At: time.Now(),
	})
	s.Record(Measurement{
		ModelID: "m1", NodeID: "coord", Role: RoleCoordinator,
		ComputeMs: MeasureQS(56.25, "joint run timing"), NetworkPath: "coord->w1",
		RTTMs: MeasureQS(10, "path probe"), BandwidthMbps: MeasureQS(5, "path probe"),
		RTTRounds: 8, RPCConnected: true, TensorBytes: MeasureQ(896),
		OverheadMs: MeasureQ(10), Success: true, Source: "joint run timing",
	})
	s.Record(Measurement{
		ModelID: "m1", NodeID: "w1", Role: RoleWorker,
		ComputeMs: MeasureQS(45, "joint run timing"), NetworkPath: "coord->w1",
		RPCConnected: true, Success: true, Source: "joint run timing",
	})
	return s
}

func distributedAssignment() *Assignment {
	return &Assignment{Kind: "distributed", NodeID: "coord",
		Cohort: []CohortMember{{NodeID: "w1", RpcAddr: "10.0.0.2:5000"}}}
}

// mergeReady layers registry model-readiness (both participants) over
// store-built inputs, exactly as the API shadow hook does.
func mergeReady(t *testing.T, in *AdmissionInputs) {
	t.Helper()
	reg := registry.New()
	reg.Register(&registry.Node{ID: "coord", Models: []string{"m1"}, LastHeartbeat: time.Now()})
	reg.Register(&registry.Node{ID: "w1", Models: []string{"m1"}, LastHeartbeat: time.Now()})
	rin := InputsFromRegistry(reg, Request{ModelID: "m1"}, distributedAssignment())
	MergeRegistryGates(in, rin)
	if !in.Gates.ModelReady {
		t.Fatal("fixture registry must yield model_ready")
	}
}

// The full pipeline: measurements -> inputs -> decision. The candidate
// is experiment-eligible but NOT production-verified (no proof record).
func TestMeasurementPipelineToDecision(t *testing.T) {
	store := fullMeasurementSet()
	in := BuildAdmissionInputs(store, store, Request{ModelID: "m1"}, distributedAssignment())
	mergeReady(t, &in)
	if !in.BestSingleMs.Measured || in.BestSingleMs.Value != 225 {
		t.Fatalf("best single = %+v, want measured 225", in.BestSingleMs)
	}
	if !in.CoordinatorMs.Measured || !in.WorkerMs.Measured {
		t.Fatalf("compute not measured: %+v %+v", in.CoordinatorMs, in.WorkerMs)
	}
	if !in.Gates.RPCReachable {
		t.Fatal("rpc_reachable should be true: both ends reported connected")
	}
	d := DecideAdmission(in, 10)
	if d.Mode != ModeDistributedCandidate || !d.ExperimentalEligible {
		t.Fatalf("mode=%s eligible=%v, want candidate+eligible", d.Mode, d.ExperimentalEligible)
	}
	if d.ProductionVerified {
		t.Fatal("timing measurements alone must never production-verify")
	}
	// Adding a proof record flips only the production tier.
	store.RecordProof(ProofRecord{ModelID: "m1", EvidenceRef: "phase-b-proof-fixture"})
	in2 := BuildAdmissionInputs(store, store, Request{ModelID: "m1"}, distributedAssignment())
	mergeReady(t, &in2)
	d2 := DecideAdmission(in2, 10)
	if !d2.ProductionVerified {
		t.Fatal("proof record with evidence ref must production-verify")
	}
	if in2.Gates.ProofEvidence != "phase-b-proof-fixture" {
		t.Fatalf("evidence ref lost: %q", in2.Gates.ProofEvidence)
	}
}

func TestMeasurementGapsStayUnmeasured(t *testing.T) {
	// No worker measurement at all.
	s := NewMemoryMeasurementStore(16)
	s.Record(Measurement{ModelID: "m1", NodeID: "coord", Role: RoleCoordinator,
		ComputeMs: MeasureQ(50), Success: true})
	in := BuildAdmissionInputs(s, s, Request{ModelID: "m1"}, distributedAssignment())
	if in.WorkerMs.Measured {
		t.Error("worker_ms must stay unmeasured without a worker record")
	}
	if d := DecideAdmission(in, 10); d.Mode != ModeInsufficientData {
		t.Errorf("decision = %s, want insufficient_data", d.Mode)
	}

	// Failed runs are ignored, including their (meaningless) timings.
	s2 := fullMeasurementSet()
	s2.Record(Measurement{ModelID: "m1", NodeID: "w1", Role: RoleWorker,
		ComputeMs: MeasureQ(1), Success: false, ErrorType: ErrTypeTimeout})
	in2 := BuildAdmissionInputs(s2, s2, Request{ModelID: "m1"}, distributedAssignment())
	if in2.WorkerMs.Value != 45 {
		t.Errorf("failed worker run polluted worker_ms: %v", in2.WorkerMs.Value)
	}

	// Network path mismatch: quantities rejected.
	s3 := fullMeasurementSet()
	s3.Record(Measurement{ModelID: "m1", NodeID: "w1", Role: RoleWorker,
		ComputeMs: MeasureQ(45), NetworkPath: "other-path", RPCConnected: true, Success: true})
	// Latest worker record now disagrees on the path.
	in3 := BuildAdmissionInputs(s3, s3, Request{ModelID: "m1"}, distributedAssignment())
	if in3.RTTMs.Measured || in3.BandwidthMbps.Measured {
		t.Error("network quantities from mismatched paths must be rejected")
	}
	if !strings.Contains(strings.Join(in3.Notes, " "), "same path") {
		t.Error("rejection must be explained in notes")
	}

	// RPC connected on one side only: gate stays closed.
	s4 := NewMemoryMeasurementStore(16)
	s4.Record(Measurement{ModelID: "m1", NodeID: "coord", Role: RoleCoordinator,
		ComputeMs: MeasureQ(50), NetworkPath: "p", RPCConnected: true, Success: true})
	s4.Record(Measurement{ModelID: "m1", NodeID: "w1", Role: RoleWorker,
		ComputeMs: MeasureQ(40), NetworkPath: "p", RPCConnected: false, Success: true})
	in4 := BuildAdmissionInputs(s4, s4, Request{ModelID: "m1"}, distributedAssignment())
	if in4.Gates.RPCReachable {
		t.Error("rpc_reachable requires BOTH ends connected")
	}

	// Multi-worker cohort: not aggregated in v1.
	a2 := &Assignment{Kind: "distributed", NodeID: "coord",
		Cohort: []CohortMember{{NodeID: "w1"}, {NodeID: "w2"}}}
	in5 := BuildAdmissionInputs(fullMeasurementSet(), nil, Request{ModelID: "m1"}, a2)
	if in5.WorkerMs.Measured {
		t.Error("multi-worker worker_ms must stay unmeasured in v1")
	}
}

// TestRealMeasurementFilePipeline feeds a REAL measurement file (from
// service/cmd/admission-probe against a local llama-server) through
// the whole collect -> store -> build -> decide path. It runs only
// when ADMISSION_MEASUREMENT_JSON points at a measurement file, so CI
// without an engine skips it; the captured run is kept as evidence.
//
// Expected outcome with only a single-node measurement: the baseline
// becomes measured, and the decision is still insufficient_data —
// the joint side has no measurements, and nothing may invent them.
func TestRealMeasurementFilePipeline(t *testing.T) {
	path := os.Getenv("ADMISSION_MEASUREMENT_JSON")
	if path == "" {
		t.Skip("ADMISSION_MEASUREMENT_JSON not set (no local engine measurement captured)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Measurement
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("measurement file does not decode: %v", err)
	}
	if !m.Success || m.Role != RoleSingle {
		t.Fatalf("expected a successful single-node measurement, got success=%v role=%q err=%q", m.Success, m.Role, m.ErrorType)
	}
	if !m.WallMs.Measured || m.ModelWeightsHash == "" {
		t.Fatalf("real measurement must carry wall time and weights hash: %+v", m)
	}
	store := NewMemoryMeasurementStore(16)
	store.Record(m)
	a := &Assignment{Kind: "remote", NodeID: m.NodeID}
	in := BuildAdmissionInputs(store, store, Request{ModelID: m.ModelID}, a)
	if !in.BestSingleMs.Measured {
		t.Fatal("best_single_ms must be measured from the real run")
	}
	if in.BestSingleMs.Value != m.WallMs.Value {
		t.Fatalf("best_single_ms = %v, want the recorded wall time %v", in.BestSingleMs.Value, m.WallMs.Value)
	}
	d := DecideAdmission(in, 10)
	if d.Mode != ModeInsufficientData {
		t.Fatalf("decision = %s, want insufficient_data (joint side unmeasured)", d.Mode)
	}
	for _, want := range []string{"coordinator_ms", "worker_ms"} {
		if !strings.Contains(strings.Join(d.Missing, " "), want) {
			t.Errorf("missing list should name %s: %v", want, d.Missing)
		}
	}
	t.Logf("real measurement: wall %.1fms, %d+%d tokens, weights sha256 %s…",
		m.WallMs.Value, m.PromptTokens, m.GenTokens, m.ModelWeightsHash[:12])
}

func TestMeasurementStoreBounds(t *testing.T) {
	s := NewMemoryMeasurementStore(2)
	for i := 0; i < 4; i++ {
		s.Record(Measurement{ModelID: "m1", Role: RoleSingle, Success: true})
	}
	if got := len(s.ForModel("m1")); got != 2 {
		t.Fatalf("store len = %d, want 2 (bounded)", got)
	}
	if got := len(s.ForModel("other")); got != 0 {
		t.Fatalf("unknown model len = %d, want 0", got)
	}
}

// End-to-end with the registry merge: model readiness comes from the
// registry (both participants), measurements supply the rest.
func TestMeasurementMergeWithRegistry(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.Node{ID: "coord", Models: []string{"m1"}, LastHeartbeat: time.Now()})
	reg.Register(&registry.Node{ID: "w1", Models: []string{"m1"}, LastHeartbeat: time.Now()})
	a := distributedAssignment()
	rin := InputsFromRegistry(reg, Request{ModelID: "m1"}, a)
	store := fullMeasurementSet()
	in := BuildAdmissionInputs(store, store, Request{ModelID: "m1"}, a)
	MergeRegistryGates(&in, rin)
	if !in.Gates.ModelReady {
		t.Fatal("model_ready must come from the registry merge (both hold m1)")
	}
	d := DecideAdmission(in, 10)
	if d.Mode != ModeDistributedCandidate {
		t.Fatalf("merged decision = %s (%s), want candidate", d.Mode, d.Reason)
	}
	// Worker drops the model -> gate closes -> single.
	reg.Register(&registry.Node{ID: "w1", Models: []string{}, LastHeartbeat: time.Now()})
	rin2 := InputsFromRegistry(reg, Request{ModelID: "m1"}, a)
	in2 := BuildAdmissionInputs(store, store, Request{ModelID: "m1"}, a)
	MergeRegistryGates(&in2, rin2)
	if in2.Gates.ModelReady {
		t.Fatal("model_ready must close when the worker drops the model")
	}
	if d2 := DecideAdmission(in2, 10); d2.Mode != ModeSingle {
		t.Fatalf("decision after model drop = %s, want single", d2.Mode)
	}
}
