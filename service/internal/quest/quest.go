// Package quest automates the Model Quest: the network's progression toward
// bigger models, computed from REAL resource snapshots — never mock numbers.
//
// Every evaluation reads the live registry (aggregate shared CPU/RAM/SSD,
// verified compute nodes, active models) and derives each ladder model's
// state:
//
//	ACTIVE   — the model file is verified on disk (registry says ACTIVE)
//	ELIGIBLE — network meets all requirements, model not yet active
//	LOCKED   — requirements not met; progress shows the bottleneck ratio
//
// States are derived fresh on every call: no stored state to go stale, no
// manual flags. (PREPARING/REPLICATING/READY/DEGRADED are M6 replica-scaling
// states, not computed here.)
//
// Requirement numbers are v1 estimates — replace with benchmark results per
// the dev plan ("실제 수치는 벤치마크 결과에 따라 정한다").
package quest

import (
	"distributed-ai/service/internal/registry"
)

// Requirements is what the network must provide for a model to unlock.
type Requirements struct {
	StorageBytes int64 `json:"storage_bytes"` // aggregate shared SSD
	RAMBytes     int64 `json:"ram_bytes"`     // aggregate shared RAM
	ComputeNodes int   `json:"compute_nodes"` // verified direct compute nodes
	TotalNodes   int   `json:"total_nodes"`   // online nodes
}

// QuestModel is one rung of the ladder.
type QuestModel struct {
	ModelID      string       `json:"model_id"`
	Requirements Requirements `json:"requirements"`
}

// Ladder is the model progression, smallest first.
var Ladder = []QuestModel{
	{
		ModelID: "qwen2.5-0.5b-q4",
		Requirements: Requirements{
			StorageBytes: 2_000_000_000, // 0.5GB file × 3 replicas + headroom
			RAMBytes:     2_000_000_000, // ~1GB inference × 2 nodes
			ComputeNodes: 1,
			TotalNodes:   1,
		},
	},
	{
		ModelID: "qwen2.5-1.5b-q4",
		Requirements: Requirements{
			StorageBytes: 4_000_000_000, // ~1GB file × 3
			RAMBytes:     6_000_000_000, // ~2.5GB inference × 2
			ComputeNodes: 2,
			TotalNodes:   3,
		},
	},
	{
		ModelID: "qwen2.5-3b-q4",
		Requirements: Requirements{
			StorageBytes: 8_000_000_000, // ~1.9GB file × 3
			RAMBytes:     10_000_000_000,
			ComputeNodes: 2,
			TotalNodes:   3,
		},
	},
	{
		ModelID: "qwen2.5-7b-q4",
		Requirements: Requirements{
			StorageBytes: 16_000_000_000, // ~4.7GB file × 3
			RAMBytes:     20_000_000_000, // ~8GB inference × 2
			ComputeNodes: 3,
			TotalNodes:   5,
		},
	},
	{
		ModelID: "qwen2.5-14b-q4", // "Large"
		Requirements: Requirements{
			StorageBytes: 30_000_000_000, // ~9GB file × 3
			RAMBytes:     40_000_000_000, // ~16GB inference × 2
			ComputeNodes: 4,
			TotalNodes:   7,
		},
	},
}

// Actual is the measured network snapshot a model is evaluated against.
type Actual struct {
	StorageBytes int64 `json:"storage_bytes"`
	RAMBytes     int64 `json:"ram_bytes"`
	ComputeNodes int   `json:"compute_nodes"`
	TotalNodes   int   `json:"total_nodes"`
}

// Entry is one ladder rung's evaluated state.
type Entry struct {
	ModelID      string       `json:"model_id"`
	State        string       `json:"state"`    // ACTIVE, ELIGIBLE, LOCKED
	Progress     float64      `json:"progress"` // 0..1, bottleneck ratio
	Requirements Requirements `json:"requirements"`
	Actual       Actual       `json:"actual"`
}

// Snapshot reads the live registry totals.
func Snapshot(reg *registry.Registry) Actual {
	totals := reg.Totals()
	storage, _ := totals["ssd_bytes"].(int64)
	ram, _ := totals["ram_bytes"].(int64)
	return Actual{
		StorageBytes: storage,
		RAMBytes:     ram,
		ComputeNodes: len(reg.ComputeNodes()),
		TotalNodes:   len(reg.OnlineNodes()),
	}
}

// ratio caps actual/required at 1; a zero requirement is always satisfied.
func ratio(actual, required int64) float64 {
	if required <= 0 {
		return 1
	}
	r := float64(actual) / float64(required)
	if r > 1 {
		return 1
	}
	if r < 0 {
		return 0
	}
	return r
}

// Progress is the bottleneck ratio across all requirements.
func (q QuestModel) Progress(a Actual) float64 {
	p := ratio(a.StorageBytes, q.Requirements.StorageBytes)
	for _, r := range []float64{
		ratio(a.RAMBytes, q.Requirements.RAMBytes),
		ratio(int64(a.ComputeNodes), int64(q.Requirements.ComputeNodes)),
		ratio(int64(a.TotalNodes), int64(q.Requirements.TotalNodes)),
	} {
		if r < p {
			p = r
		}
	}
	return p
}

// Evaluate derives every rung's state from the live registry.
func Evaluate(reg *registry.Registry) []Entry {
	actual := Snapshot(reg)
	active := map[string]bool{}
	for _, m := range reg.Models() {
		if m.Status == "ACTIVE" {
			active[m.ModelID] = true
		}
	}
	out := make([]Entry, 0, len(Ladder))
	for _, q := range Ladder {
		e := Entry{
			ModelID:      q.ModelID,
			Requirements: q.Requirements,
			Actual:       actual,
			Progress:     q.Progress(actual),
		}
		switch {
		case active[q.ModelID]:
			e.State = "ACTIVE"
			e.Progress = 1
		case e.Progress >= 1:
			e.State = "ELIGIBLE"
		default:
			e.State = "LOCKED"
		}
		out = append(out, e)
	}
	return out
}
