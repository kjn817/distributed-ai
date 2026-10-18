package placement

import (
	"distributed-ai/service/internal/registry"
)

// DistributedGroup forms a worker group for joint compute: one coordinator
// plus N rpc-server workers jointly compute ONE request via llama.cpp RPC.
// This is distinct from request routing (SingleNodeFirst), which sends
// different requests to different nodes.
//
// Requirements:
//   - Coordinator: a compute node holding the model (runs llama-server --rpc).
//   - Workers: nodes with RpcAddr set (running rpc-server) holding the model.
//   - The coordinator must be able to TCP-dial the workers' RpcAddr
//     (public IP or port-forwarded; NAT workers can't serve as RPC backends).
type DistributedGroup struct {
	// MinWorkers is the minimum workers required (excluding coordinator).
	MinWorkers int
}

func (d DistributedGroup) Place(reg *registry.Registry, localReady bool, req Request) (*Assignment, error) {
	if d.MinWorkers <= 0 {
		d.MinWorkers = 1
	}
	// Workers: nodes with an RPC address holding the model. DirectOK is
	// NOT required: the coordinator dials the worker directly (TCP), not
	// via the service. The service cannot verify coordinator→worker
	// reachability (documented limitation); the RpcAddr is trusted.
	var workers []*registry.Node
	for _, n := range reg.OnlineNodes() {
		if n.RpcAddr == "" {
			continue
		}
		if !holdsModel(n, req.ModelID) {
			continue
		}
		workers = append(workers, n)
	}
	if len(workers) < d.MinWorkers {
		return nil, ErrNoCapacity
	}
	// Coordinator: best compute node holding the model, preferring a
	// non-worker to spread load.
	var coord *registry.Node
	bestScore := -1e18
	for _, n := range reg.ComputeNodes() {
		if !holdsModel(n, req.ModelID) {
			continue
		}
		s := Score(n)
		if isWorkerNode(n.ID, workers) {
			s -= 1000 // prefer non-workers as coordinator
		}
		if s > bestScore {
			bestScore, coord = s, n
		}
	}
	if coord == nil {
		return nil, ErrNoCapacity
	}
	cohort := make([]CohortMember, 0, len(workers))
	for _, w := range workers {
		if w.ID == coord.ID {
			continue
		}
		cohort = append(cohort, CohortMember{NodeID: w.ID, RpcAddr: w.RpcAddr})
	}
	return &Assignment{Kind: "distributed", NodeID: coord.ID, Cohort: cohort}, nil
}

func holdsModel(n *registry.Node, modelID string) bool {
	for _, m := range n.Models {
		if m == modelID {
			return true
		}
	}
	return false
}

func isWorkerNode(id string, workers []*registry.Node) bool {
	for _, w := range workers {
		if w.ID == id {
			return true
		}
	}
	return false
}
