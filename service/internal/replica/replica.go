// Package replica plans model replication from real state.
//
// For each known model: desired holders come from the manifest's
// replication target (bumped when measured demand is hot); actual holders
// are counted live. When actual < desired, the planner picks verified
// candidate nodes and the service asks them — via heartbeat — to fetch the
// model over P2P. Healing (a holder left) and demand scale-up are the same
// code path: the network converges toward the desired count.
package replica

import (
	"distributed-ai/service/internal/metrics"
	"distributed-ai/service/internal/registry"
)

// hotRPS bumps the desired replica count: sustained demand above this
// requests-per-second wants one more copy. v1 heuristic.
const hotRPS = 0.1

// Assignment asks one node to fetch one model.
type Assignment struct {
	ModelID string `json:"model_id"`
	NodeID  string `json:"node_id"`
}

// Status is one model's replication state (for the dashboard/API).
type Status struct {
	ModelID string  `json:"model_id"`
	Holders int     `json:"holders"`
	Desired int     `json:"desired"`
	RPS     float64 `json:"rps_5m"`
	Healthy bool    `json:"healthy"`
}

// desiredHolders derives the target count from the manifest and demand.
func desiredHolders(reg *registry.Registry, tr *metrics.Tracker, modelID string) int {
	desired := 2 // sane default when the manifest says nothing
	if mf, ok := reg.Manifest(modelID); ok {
		if mf.Replication.Target > 0 {
			desired = mf.Replication.Target
		} else if mf.Replication.Min > 0 {
			desired = mf.Replication.Min
		}
	}
	if tr.RPS(modelID) >= hotRPS {
		desired++
	}
	return desired
}

// Plan returns replication assignments to converge toward desired counts.
// Only models with at least one holder and chunked manifests are
// actionable — with nothing to fetch from, there is nothing to assign.
func Plan(reg *registry.Registry, tr *metrics.Tracker) []Assignment {
	var out []Assignment
	for _, m := range reg.Models() {
		holders := reg.ModelHolders(m.ModelID)
		desired := desiredHolders(reg, tr, m.ModelID)
		if len(holders) >= desired {
			continue
		}
		mf, ok := reg.Manifest(m.ModelID)
		if !ok || len(mf.Chunks.List) == 0 || len(holders) == 0 {
			continue // nothing to fetch from; planner cannot help
		}
		holderSet := map[string]bool{}
		for _, h := range holders {
			holderSet[h.ID] = true
		}
		need := desired - len(holders)
		for _, n := range reg.ComputeNodes() {
			if need <= 0 {
				break
			}
			if holderSet[n.ID] {
				continue
			}
			if n.SSDSharedBytes < mf.SizeBytes {
				continue // never ask a node to store more than it offered
			}
			out = append(out, Assignment{ModelID: m.ModelID, NodeID: n.ID})
			holderSet[n.ID] = true // don't assign the same node twice this round
			need--
		}
	}
	return out
}

// Statuses reports per-model replication health.
func Statuses(reg *registry.Registry, tr *metrics.Tracker) []Status {
	var out []Status
	for _, m := range reg.Models() {
		holders := len(reg.ModelHolders(m.ModelID))
		desired := desiredHolders(reg, tr, m.ModelID)
		out = append(out, Status{
			ModelID: m.ModelID,
			Holders: holders,
			Desired: desired,
			RPS:     tr.RPS(m.ModelID),
			Healthy: holders >= desired,
		})
	}
	return out
}
