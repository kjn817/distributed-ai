// Package placement decides where an inference request runs.
//
// Compute Placement (per docs/manifest-spec.md rev2): which worker(s) handle
// a request, based on worker capability, memory, measured RTT, and the
// model's execution strategy. The mechanism is strategy-swappable; v1 fixes
// the policy to single-node-first — the defensive default, because
// per-request multi-node inference over the internet is latency-bound and
// multi-node execution is fallback-only.
//
// Future distributed strategies (DISTRIBUTED_MATERIALIZE) implement the
// Strategy interface and fill Assignment.Cohort with per-worker layer/tensor
// ranges. They need RPC-capable engines, which v1 does not ship.
package placement

import (
	"errors"
	"time"

	"distributed-ai/service/internal/registry"
)

// Strategies understood by the placement layer.
const (
	StrategyFullMaterialize        = "FULL_MATERIALIZE"
	StrategyDistributedMaterialize = "DISTRIBUTED_MATERIALIZE"
)

// ErrNoCapacity means nothing can serve the request right now.
var ErrNoCapacity = errors.New("placement: no capacity")

// ErrStrategyUnsupported means the model's execution strategy has no
// placement implementation yet (e.g. DISTRIBUTED_MATERIALIZE needs
// RPC-capable engines — honest 501, not a silent fallback).
var ErrStrategyUnsupported = errors.New("placement: execution strategy not supported")

// Request describes one inference request's placement needs.
type Request struct {
	ModelID  string
	Strategy string // execution strategy from the model manifest
}

// CohortMember is one worker in a distributed assignment.
type CohortMember struct {
	NodeID string `json:"node_id"`
	// RpcAddr is "host:port" of the worker's rpc-server (for --rpc).
	RpcAddr string `json:"rpc_addr,omitempty"`
}

// Assignment is where the request goes.
//   - "remote": one node computes (request routing).
//   - "distributed": coordinator (NodeID) + Cohort workers jointly compute
//     one request via llama.cpp RPC (joint compute).
type Assignment struct {
	Kind   string         `json:"kind"` // "local", "remote", or "distributed"
	NodeID string         `json:"node_id,omitempty"`
	Cohort []CohortMember `json:"cohort,omitempty"`
}

// Strategy picks the assignment for a request.
type Strategy interface {
	Place(reg *registry.Registry, localReady bool, req Request) (*Assignment, error)
}

// Score rates a compute node. Higher is better.
// All inputs are real: shared CPU cores, shared RAM, GPU presence,
// heartbeat freshness, measured dial-back RTT (lower is better), and
// current in-flight dispatches (fewer is better — this is what spreads
// concurrent requests across replicas).
// Weights are v1 heuristic constants, tuned later against real data.
func Score(n *registry.Node) float64 {
	s := float64(n.CPUCores)*1.0 +
		float64(n.RAMBytes)/1e9*0.5
	if n.GPUModel != "" {
		s += 10
	}
	// Prefer recently seen nodes.
	if time.Since(n.LastHeartbeat) > 60*time.Second {
		s -= 5
	}
	// Prefer low-latency nodes: 1 score point per RTT-second.
	s -= float64(n.RTTMs) / 1000.0
	// Prefer unloaded nodes: one in-flight request costs ~2 CPU cores.
	s -= float64(n.InFlight) * 2.0
	return s
}

// SingleNodeFirst is the v1 strategy: the best-scoring verified remote
// compute node wins when one exists (volunteers serve and earn); otherwise
// the local engine serves; otherwise no capacity.
type SingleNodeFirst struct{}

// Place implements Strategy.
func (SingleNodeFirst) Place(reg *registry.Registry, localReady bool, req Request) (*Assignment, error) {
	switch req.Strategy {
	case "", StrategyFullMaterialize:
		// supported
	default:
		return nil, ErrStrategyUnsupported
	}
	var best *registry.Node
	bestScore := -1e18
	for _, n := range reg.ComputeNodes() {
		if s := Score(n); s > bestScore {
			bestScore, best = s, n
		}
	}
	if best != nil {
		return &Assignment{Kind: "remote", NodeID: best.ID}, nil
	}
	if localReady {
		return &Assignment{Kind: "local"}, nil
	}
	return nil, ErrNoCapacity
}
