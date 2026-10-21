package placement

// Adaptive Compute Admission (experiment, shadow mode only).
//
// This file adds a measurement-driven comparison of single-node vs joint
// (coordinator + RPC worker) execution to the placement package. It is a
// pure function plus an in-memory recorder:
//
//   - DecideAdmission NEVER dispatches, dials, or mutates anything.
//   - Every quantity carries a Measured flag. A value that was not actually
//     measured MUST be passed as Unmeasured(); the decision then degrades
//     to ModeInsufficientData and lists what is missing. Estimated or
//     synthetic numbers are never presented as measurements.
//   - ModeDistributedCandidate is a prediction, not evidence that joint
//     execution works. Joint execution on DAIN is still unverified
//     end-to-end (see docs/phase-b-proof.md), so the gates below require
//     explicit proof inputs before a candidate is even suggested.
//
// Integration: the API layer may record decisions in a ShadowRecorder for
// observation. The recorder is OFF by default (nil) and the recorded
// decision is never used to choose the assignment.

import (
	"fmt"
	"math"
	"sync"
	"time"

	"distributed-ai/service/internal/registry"
)

// Admission modes.
const (
	ModeSingle               = "single"
	ModeDistributedCandidate = "distributed_candidate"
	ModeInsufficientData     = "insufficient_data"
)

// Quantity is one numeric input with its provenance. Measured=false means
// "we do not have a real measurement" — the value field is then ignored.
type Quantity struct {
	Value    float64 `json:"value"`
	Measured bool    `json:"measured"`
}

// MeasureQ marks v as a real measurement.
func MeasureQ(v float64) Quantity { return Quantity{Value: v, Measured: true} }

// UnmeasuredQ marks a quantity as not measured.
func UnmeasuredQ() Quantity { return Quantity{} }

// JointGates are the proof preconditions for even considering joint
// execution. They are booleans about verified facts, not measurements.
type JointGates struct {
	// RPCReachable: a coordinator→worker RPC connection has been verified.
	// The service cannot verify this today (see DistributedGroup), so the
	// registry-derived inputs always leave it false.
	RPCReachable bool `json:"rpc_reachable"`
	// ModelReady: coordinator and workers hold the model, ready to run.
	ModelReady bool `json:"model_ready"`
	// JointExecutionSafe: joint execution has been proven to actually
	// offload (e.g. -ngl evidence on both sides), not merely to connect.
	JointExecutionSafe bool `json:"joint_execution_safe"`
}

// AdmissionInputs are the measurements one admission decision needs.
// All times are milliseconds; bandwidth is megabits per second.
type AdmissionInputs struct {
	RequestID string `json:"request_id"`
	ModelID   string `json:"model_id,omitempty"`

	// BestSingleMs: measured end-to-end time of the fastest feasible
	// single node for this request shape (the honest baseline).
	BestSingleMs Quantity `json:"best_single_ms"`
	// CoordinatorMs / WorkerMs: measured compute time attributable to the
	// coordinator side and the worker side in a joint run.
	CoordinatorMs Quantity `json:"coordinator_ms"`
	WorkerMs      Quantity `json:"worker_ms"`
	// RTTMs: measured round-trip time on the coordinator→worker path.
	RTTMs Quantity `json:"rtt_ms"`
	// RTTRounds: how many RTT-priced exchanges the joint protocol needs
	// for this request (known from the protocol, not measured).
	RTTRounds      int  `json:"rtt_rounds"`
	RTTRoundsKnown bool `json:"rtt_rounds_known"`
	// PayloadBytes: tensor/activation bytes exchanged with the worker.
	PayloadBytes Quantity `json:"payload_bytes"`
	// BandwidthMbps: measured coordinator→worker bandwidth.
	BandwidthMbps Quantity `json:"bandwidth_mbps"`
	// OverheadMs: other measured overhead (serialization, engine restart
	// amortization, scheduling).
	OverheadMs Quantity `json:"overhead_ms"`

	Gates JointGates `json:"gates"`
	// Notes document provenance caveats (e.g. an RTT measured on a
	// different path than the one it is used for). They are part of the
	// recorded evidence, never decoration.
	Notes []string `json:"notes,omitempty"`
}

// AdmissionDecision is the recorded outcome. Predictions are pointers so
// that "not computed" (nil) is distinguishable from a real 0.
type AdmissionDecision struct {
	RequestID    string          `json:"request_id"`
	ModelID      string          `json:"model_id,omitempty"`
	Mode         string          `json:"mode"`
	Reason       string          `json:"reason"`
	Missing      []string        `json:"missing,omitempty"`
	Inputs       AdmissionInputs `json:"inputs"`
	MinSavingPct float64         `json:"min_saving_pct"`

	PredictedSingleMs  *float64 `json:"predicted_single_ms,omitempty"`
	PredictedJointMs   *float64 `json:"predicted_joint_ms,omitempty"`
	TransferMs         *float64 `json:"transfer_ms,omitempty"`
	PredictedSavingPct *float64 `json:"predicted_saving_pct,omitempty"`

	// Shadow is always true: this decision was recorded for observation
	// and did not influence placement.
	Shadow bool `json:"shadow"`
}

func validPositive(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 }
func validNonneg(v float64) bool   { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func fptr(v float64) *float64 { return &v }

// DecideAdmission compares predicted single-node vs joint execution from
// measured inputs only. It is deterministic and side-effect free.
func DecideAdmission(in AdmissionInputs, minSavingPct float64) AdmissionDecision {
	d := AdmissionDecision{
		RequestID:    in.RequestID,
		ModelID:      in.ModelID,
		Mode:         ModeInsufficientData,
		Inputs:       in,
		MinSavingPct: minSavingPct,
		Shadow:       true,
	}

	// The single-node baseline is reported whenever it is genuinely
	// measured, even if the joint side cannot be evaluated.
	if in.BestSingleMs.Measured && validPositive(in.BestSingleMs.Value) {
		d.PredictedSingleMs = fptr(in.BestSingleMs.Value)
	}

	if !validNonneg(minSavingPct) || minSavingPct >= 100 {
		d.Reason = "invalid min_saving_pct: must be finite, >= 0 and < 100"
		d.Missing = []string{"min_saving_pct"}
		return d
	}

	type check struct {
		name string
		q    Quantity
		need func(float64) bool
	}
	checks := []check{
		{"best_single_ms", in.BestSingleMs, validPositive},
		{"coordinator_ms", in.CoordinatorMs, validPositive},
		{"worker_ms", in.WorkerMs, validPositive},
		{"rtt_ms", in.RTTMs, validNonneg},
		{"payload_bytes", in.PayloadBytes, validNonneg},
		{"bandwidth_mbps", in.BandwidthMbps, validPositive},
		{"overhead_ms", in.OverheadMs, validNonneg},
	}
	for _, c := range checks {
		if !c.q.Measured {
			d.Missing = append(d.Missing, c.name+":unmeasured")
		} else if !c.need(c.q.Value) {
			d.Missing = append(d.Missing, c.name+":invalid")
		}
	}
	if !in.RTTRoundsKnown {
		d.Missing = append(d.Missing, "rtt_rounds:unknown")
	} else if in.RTTRounds < 1 {
		d.Missing = append(d.Missing, "rtt_rounds:invalid")
	}
	if len(d.Missing) > 0 {
		d.Reason = "missing or invalid measured inputs: joint execution cannot be evaluated honestly"
		return d
	}

	// Data is sufficient; the joint path still needs proven gates.
	var closed []string
	if !in.Gates.RPCReachable {
		closed = append(closed, "rpc_reachable")
	}
	if !in.Gates.ModelReady {
		closed = append(closed, "model_ready")
	}
	if !in.Gates.JointExecutionSafe {
		closed = append(closed, "joint_execution_safe")
	}
	if len(closed) > 0 {
		d.Mode = ModeSingle
		d.Reason = fmt.Sprintf("joint path not proven: %v", closed)
		return d
	}

	transferMs := in.PayloadBytes.Value * 8 / (in.BandwidthMbps.Value * 1e3)
	joint := in.CoordinatorMs.Value + in.WorkerMs.Value +
		in.RTTMs.Value*float64(in.RTTRounds) + transferMs + in.OverheadMs.Value
	saving := 100 * (1 - joint/in.BestSingleMs.Value)
	d.TransferMs = fptr(transferMs)
	d.PredictedJointMs = fptr(joint)
	d.PredictedSavingPct = fptr(saving)
	if saving >= minSavingPct {
		d.Mode = ModeDistributedCandidate
		d.Reason = "predicted joint execution clears the minimum improvement threshold (prediction only — not execution evidence)"
	} else {
		d.Mode = ModeSingle
		d.Reason = "predicted joint execution does not improve enough over the best single node"
	}
	return d
}

// InputsFromRegistry derives admission inputs from what the registry
// actually knows. It deliberately leaves most quantities unmeasured:
//
//   - The registry has NO per-node inference timings and NO bandwidth
//     measurements, so compute/transfer inputs stay unmeasured and the
//     live decision is ModeInsufficientData until a measurement source
//     exists. That is the honest state, not a defect to paper over.
//   - RTTMs comes from the service's dial-back measurement, which is the
//     service↔node path, not coordinator↔worker; the note says so.
//   - RPCReachable stays false: RpcAddr presence is a claim, and the
//     service cannot verify coordinator→worker reachability.
//   - JointExecutionSafe stays false: no joint-execution evidence source
//     is wired into the service.
func InputsFromRegistry(reg *registry.Registry, req Request, a *Assignment) AdmissionInputs {
	in := AdmissionInputs{RequestID: "", ModelID: req.ModelID}
	in.Notes = append(in.Notes,
		"compute timings, bandwidth, payload and rtt_rounds have no measurement source in the service yet — left unmeasured on purpose",
		"rpc_reachable is never inferred from a registered rpc_addr claim",
		"joint_execution_safe has no evidence source wired into the service",
	)
	if a != nil {
		in.Notes = append(in.Notes, "actual assignment kind (unchanged by admission): "+a.Kind)
	}
	var node *registry.Node
	if a != nil && a.NodeID != "" && reg != nil {
		node = reg.Get(a.NodeID)
	}
	if node != nil {
		if node.DirectOK && node.RTTMs > 0 {
			in.RTTMs = MeasureQ(float64(node.RTTMs))
			in.Notes = append(in.Notes, "rtt_ms is the service dial-back RTT (service↔node), not a coordinator↔worker measurement")
		}
		if holdsModel(node, req.ModelID) {
			in.Gates.ModelReady = true
		}
	}
	return in
}

// ShadowRecord pairs a decision with when it was recorded.
type ShadowRecord struct {
	At       time.Time         `json:"at"`
	Decision AdmissionDecision `json:"decision"`
}

// ShadowRecorder is a bounded in-memory log of admission decisions.
// It has no effect on placement; it exists so the shadow predictions can
// later be compared against real outcomes.
type ShadowRecorder struct {
	mu       sync.Mutex
	capacity int
	records  []ShadowRecord
}

// NewShadowRecorder creates a recorder keeping at most capacity records
// (default 128 when capacity <= 0).
func NewShadowRecorder(capacity int) *ShadowRecorder {
	if capacity <= 0 {
		capacity = 128
	}
	return &ShadowRecorder{capacity: capacity}
}

// Record appends a decision, evicting the oldest beyond capacity.
func (r *ShadowRecorder) Record(d AdmissionDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, ShadowRecord{At: time.Now(), Decision: d})
	if len(r.records) > r.capacity {
		r.records = append([]ShadowRecord(nil), r.records[len(r.records)-r.capacity:]...)
	}
}

// Snapshot returns a copy of the recorded decisions, oldest first.
func (r *ShadowRecorder) Snapshot() []ShadowRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ShadowRecord(nil), r.records...)
}

// Len reports how many decisions are currently held.
func (r *ShadowRecorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}
