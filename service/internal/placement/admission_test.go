package placement

import (
	"math"
	"strings"
	"testing"
	"time"

	"distributed-ai/service/internal/registry"
)

// fullInputs returns a fully measured input set. Numbers are test fixtures
// (chosen for hand-checkable arithmetic), not performance claims.
func fullInputs() AdmissionInputs {
	return AdmissionInputs{
		RequestID:      "req-1",
		ModelID:        "m1",
		BestSingleMs:   MeasureQ(225),
		CoordinatorMs:  MeasureQ(56.25),
		WorkerMs:       MeasureQ(45),
		RTTMs:          MeasureQ(10),
		RTTRounds:      8,
		RTTRoundsKnown: true,
		PayloadBytes:   MeasureQ(896),
		BandwidthMbps:  MeasureQ(5),
		OverheadMs:     MeasureQ(10),
		// Experiment tier only: reachable + ready, no production proof.
		Gates: JointGates{RPCReachable: true, ModelReady: true},
	}
}

// joint = 56.25 + 45 + 10*8 + (896*8/(5*1000)=1.4336) + 10 = 192.6836
func TestAdmissionJointCandidate(t *testing.T) {
	d := DecideAdmission(fullInputs(), 10)
	if d.Mode != ModeDistributedCandidate {
		t.Fatalf("mode = %s, want distributed_candidate (%s)", d.Mode, d.Reason)
	}
	if d.PredictedJointMs == nil || math.Abs(*d.PredictedJointMs-192.6836) > 1e-9 {
		t.Fatalf("joint = %v, want 192.6836", d.PredictedJointMs)
	}
	if d.PredictedSingleMs == nil || *d.PredictedSingleMs != 225 {
		t.Fatalf("single = %v, want 225", d.PredictedSingleMs)
	}
	wantSaving := 100 * (1 - 192.6836/225)
	if d.PredictedSavingPct == nil || math.Abs(*d.PredictedSavingPct-wantSaving) > 1e-9 {
		t.Fatalf("saving = %v, want %f", d.PredictedSavingPct, wantSaving)
	}
	if !d.Shadow {
		t.Fatal("decision must be marked shadow")
	}
	// The circular dependency is gone: no production proof exists in
	// these inputs, yet the candidate is experiment-eligible — and it
	// must NOT be marked production-verified.
	if !d.ExperimentalEligible {
		t.Fatal("candidate must be experiment-eligible without production proof")
	}
	if d.ProductionVerified {
		t.Fatal("candidate without proof record must not be production-verified")
	}
	if d.Scopes["coordinator_ms"] == "" || d.Scopes["transfer_ms"] == "" {
		t.Fatal("decision must carry the measurement scope definitions")
	}
}

func TestAdmissionProductionVerifiedRequiresEvidence(t *testing.T) {
	// Boolean claim without an evidence reference: not verified.
	in := fullInputs()
	in.Gates.JointExecutionProven = true
	d := DecideAdmission(in, 10)
	if d.Mode != ModeDistributedCandidate || !d.ExperimentalEligible {
		t.Fatalf("mode=%s eligible=%v, want candidate+eligible", d.Mode, d.ExperimentalEligible)
	}
	if d.ProductionVerified {
		t.Fatal("proof boolean without evidence reference must not verify production")
	}
	// With an evidence reference: verified (still shadow; assigns nothing).
	in.Gates.ProofEvidence = "phase-b-proof-2026-10-21"
	d = DecideAdmission(in, 10)
	if !d.ProductionVerified {
		t.Fatal("proof + evidence reference must mark production-verified")
	}
}

func TestAdmissionSingleWhenJointSlower(t *testing.T) {
	in := fullInputs()
	in.BestSingleMs = MeasureQ(60) // a fast single node beats the split
	d := DecideAdmission(in, 10)
	if d.Mode != ModeSingle {
		t.Fatalf("mode = %s, want single", d.Mode)
	}
}

func TestAdmissionThresholdBoundary(t *testing.T) {
	in := fullInputs()
	// saving is ~14.36%: a threshold exactly at the saving passes (>=),
	// anything above fails to single. The expected saving is recomputed
	// with the same operation order as DecideAdmission so the boundary
	// comparison is bit-exact, not a rounded literal.
	transfer := 896.0 * 8 / (5.0 * 1e3)
	joint := 56.25 + 45 + 10.0*8 + transfer + 10
	saving := 100 * (1 - joint/225)
	if d := DecideAdmission(in, saving); d.Mode != ModeDistributedCandidate {
		t.Fatalf("at-threshold: mode = %s, want candidate", d.Mode)
	}
	if d := DecideAdmission(in, saving+0.01); d.Mode != ModeSingle {
		t.Fatalf("above-threshold: mode = %s, want single", d.Mode)
	}
	// Zero threshold: any positive saving is a candidate; a joint loss is not.
	if d := DecideAdmission(in, 0); d.Mode != ModeDistributedCandidate {
		t.Fatalf("zero-threshold: mode = %s, want candidate", d.Mode)
	}
	slow := fullInputs()
	slow.WorkerMs = MeasureQ(1000)
	if d := DecideAdmission(slow, 0); d.Mode != ModeSingle {
		t.Fatalf("zero-threshold slow joint: mode = %s, want single", d.Mode)
	}
}

func TestAdmissionUnmeasuredNeverPredicts(t *testing.T) {
	cases := map[string]func(*AdmissionInputs){
		"best_single":  func(in *AdmissionInputs) { in.BestSingleMs = UnmeasuredQ() },
		"coordinator":  func(in *AdmissionInputs) { in.CoordinatorMs = UnmeasuredQ() },
		"worker":       func(in *AdmissionInputs) { in.WorkerMs = UnmeasuredQ() },
		"rtt":          func(in *AdmissionInputs) { in.RTTMs = UnmeasuredQ() },
		"payload":      func(in *AdmissionInputs) { in.PayloadBytes = UnmeasuredQ() },
		"bandwidth":    func(in *AdmissionInputs) { in.BandwidthMbps = UnmeasuredQ() },
		"overhead":     func(in *AdmissionInputs) { in.OverheadMs = UnmeasuredQ() },
		"rtt_rounds":   func(in *AdmissionInputs) { in.RTTRoundsKnown = false },
		"gates_closed": func(in *AdmissionInputs) { in.Gates.RPCReachable = false },
	}
	for name, mutate := range cases {
		in := fullInputs()
		mutate(&in)
		d := DecideAdmission(in, 10)
		if name == "gates_closed" {
			if d.Mode != ModeSingle {
				t.Errorf("%s: mode = %s, want single", name, d.Mode)
			}
			continue
		}
		if d.Mode != ModeInsufficientData {
			t.Errorf("%s: mode = %s, want insufficient_data", name, d.Mode)
		}
		if d.PredictedJointMs != nil {
			t.Errorf("%s: joint prediction present despite missing data", name)
		}
		if len(d.Missing) == 0 {
			t.Errorf("%s: missing list empty", name)
		}
	}
}

func TestAdmissionInvalidMeasuredValues(t *testing.T) {
	bad := []struct {
		name   string
		mutate func(*AdmissionInputs)
	}{
		{"nan_single", func(in *AdmissionInputs) { in.BestSingleMs = MeasureQ(math.NaN()) }},
		{"inf_bandwidth", func(in *AdmissionInputs) { in.BandwidthMbps = MeasureQ(math.Inf(1)) }},
		{"zero_bandwidth", func(in *AdmissionInputs) { in.BandwidthMbps = MeasureQ(0) }},
		{"negative_rtt", func(in *AdmissionInputs) { in.RTTMs = MeasureQ(-1) }},
		{"negative_payload", func(in *AdmissionInputs) { in.PayloadBytes = MeasureQ(-5) }},
		{"zero_rounds", func(in *AdmissionInputs) { in.RTTRounds = 0 }},
	}
	for _, c := range bad {
		in := fullInputs()
		c.mutate(&in)
		if d := DecideAdmission(in, 10); d.Mode != ModeInsufficientData {
			t.Errorf("%s: mode = %s, want insufficient_data", c.name, d.Mode)
		}
	}
	// Zero RTT and zero overhead are legitimate measurements (local path).
	in := fullInputs()
	in.RTTMs = MeasureQ(0)
	in.OverheadMs = MeasureQ(0)
	if d := DecideAdmission(in, 10); d.Mode == ModeInsufficientData {
		t.Errorf("zero rtt/overhead wrongly rejected: %s", d.Reason)
	}
}

func TestAdmissionInvalidThreshold(t *testing.T) {
	for _, th := range []float64{-1, 100, 150, math.NaN(), math.Inf(1)} {
		if d := DecideAdmission(fullInputs(), th); d.Mode != ModeInsufficientData {
			t.Errorf("threshold %v: mode = %s, want insufficient_data", th, d.Mode)
		}
	}
}

func TestAdmissionEachGateClosedMeansSingle(t *testing.T) {
	// Only the two EXPERIMENT gates close eligibility now. The
	// production-proof gate no longer blocks a candidate (that was the
	// circular dependency); it only controls ProductionVerified.
	for _, closeGate := range []func(*JointGates){
		func(g *JointGates) { g.RPCReachable = false },
		func(g *JointGates) { g.ModelReady = false },
	} {
		in := fullInputs()
		closeGate(&in.Gates)
		d := DecideAdmission(in, 10)
		if d.Mode != ModeSingle || !strings.Contains(d.Reason, "not eligible") {
			t.Errorf("gate closed: mode=%s reason=%q, want single/not eligible", d.Mode, d.Reason)
		}
		if d.ExperimentalEligible || d.ProductionVerified {
			t.Errorf("gate closed: eligible=%v verified=%v, want both false", d.ExperimentalEligible, d.ProductionVerified)
		}
		if d.PredictedJointMs != nil {
			t.Errorf("gate closed: joint prediction must not be produced")
		}
	}
}

// InputsFromRegistry must stay honest against the real registry: no
// inference timings exist there, so the decision is insufficient_data,
// the measured dial-back RTT is used (with its caveat), and RpcAddr
// presence never flips RPCReachable.
func TestInputsFromRegistryHonestDefaults(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.Node{
		ID: "n1", CPUCores: 4, Models: []string{"m1"}, RpcAddr: "10.0.0.2:5000",
		LastHeartbeat: time.Now(),
	})
	reg.SetDirectOK("n1", true, 12*time.Millisecond)

	a := &Assignment{Kind: "remote", NodeID: "n1"}
	in := InputsFromRegistry(reg, Request{ModelID: "m1"}, a)
	if in.RTTMs != MeasureQ(12) {
		t.Errorf("rtt = %+v, want measured 12", in.RTTMs)
	}
	if !in.Gates.ModelReady {
		t.Error("model_ready should be true: node holds the model")
	}
	if in.Gates.RPCReachable {
		t.Error("rpc_reachable must stay false even with an rpc_addr registered")
	}
	if in.Gates.JointExecutionProven {
		t.Error("joint_execution_proven must stay false: no proof-record source")
	}
	if in.BestSingleMs.Measured || in.BandwidthMbps.Measured || in.CoordinatorMs.Measured {
		t.Error("compute/bandwidth inputs must stay unmeasured — the registry has none")
	}
	d := DecideAdmission(in, 10)
	if d.Mode != ModeInsufficientData {
		t.Fatalf("live-path decision = %s, want insufficient_data until measurements exist", d.Mode)
	}
	joined := strings.Join(in.Notes, " ")
	if !strings.Contains(joined, "dial-back") {
		t.Error("notes must disclose the RTT path caveat")
	}
}

// ModelReady must cover the coordinator AND every cohort worker:
// a coordinator-only check would let an unready worker into a joint run.
func TestInputsFromRegistryModelReadyBothSides(t *testing.T) {
	mk := func(workerModels []string, registerWorker bool) AdmissionInputs {
		reg := registry.New()
		reg.Register(&registry.Node{ID: "coord", CPUCores: 4, Models: []string{"m1"}, LastHeartbeat: time.Now()})
		if registerWorker {
			reg.Register(&registry.Node{ID: "w1", Models: workerModels, LastHeartbeat: time.Now()})
		}
		a := &Assignment{Kind: "distributed", NodeID: "coord",
			Cohort: []CohortMember{{NodeID: "w1", RpcAddr: "10.0.0.2:5000"}}}
		return InputsFromRegistry(reg, Request{ModelID: "m1"}, a)
	}
	if in := mk([]string{"m1"}, true); !in.Gates.ModelReady {
		t.Error("both hold the model: ModelReady must be true")
	}
	if in := mk([]string{"other"}, true); in.Gates.ModelReady {
		t.Error("worker holds a different model: ModelReady must be false")
	}
	if in := mk(nil, true); in.Gates.ModelReady {
		t.Error("worker holds nothing: ModelReady must be false")
	}
	if in := mk(nil, false); in.Gates.ModelReady {
		t.Error("worker unknown to the registry: ModelReady must be false")
	}
	// Coordinator without the model closes the gate even if workers hold it.
	reg := registry.New()
	reg.Register(&registry.Node{ID: "coord", Models: []string{"other"}, LastHeartbeat: time.Now()})
	reg.Register(&registry.Node{ID: "w1", Models: []string{"m1"}, LastHeartbeat: time.Now()})
	a := &Assignment{Kind: "distributed", NodeID: "coord", Cohort: []CohortMember{{NodeID: "w1"}}}
	if in := InputsFromRegistry(reg, Request{ModelID: "m1"}, a); in.Gates.ModelReady {
		t.Error("coordinator lacks the model: ModelReady must be false")
	}
}

func TestInputsFromRegistryNoNode(t *testing.T) {
	reg := registry.New()
	in := InputsFromRegistry(reg, Request{ModelID: "m1"}, &Assignment{Kind: "local"})
	if in.RTTMs.Measured {
		t.Error("no node => no RTT measurement")
	}
	if in.Gates.ModelReady {
		t.Error("no node => model_ready must not be claimed")
	}
}

func TestShadowRecorderBoundsAndCopies(t *testing.T) {
	r := NewShadowRecorder(3)
	for i := 0; i < 5; i++ {
		r.Record(AdmissionDecision{RequestID: string(rune('a' + i)), Mode: ModeSingle})
	}
	if r.Len() != 3 {
		t.Fatalf("len = %d, want 3 (bounded)", r.Len())
	}
	snap := r.Snapshot()
	if snap[0].Decision.RequestID != "c" || snap[2].Decision.RequestID != "e" {
		t.Fatalf("eviction order wrong: %q..%q", snap[0].Decision.RequestID, snap[2].Decision.RequestID)
	}
	snap[0].Decision.RequestID = "mutated"
	if r.Snapshot()[0].Decision.RequestID != "c" {
		t.Fatal("snapshot must be a copy")
	}
	if got := NewShadowRecorder(0).capacity; got != 128 {
		t.Fatalf("default capacity = %d, want 128", got)
	}
}
