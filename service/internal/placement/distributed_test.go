package placement

import (
	"distributed-ai/service/internal/registry"
	"testing"
)

func TestDistributedGroup(t *testing.T) {
	reg := registry.New()
	// Coordinator: no RPC, has model, DirectOK
	reg.Register(&registry.Node{ID: "coord", DirectOK: true, CPUCores: 4, Models: []string{"m1"}, RpcAddr: ""})
	// Worker: RPC, has model, DirectOK
	reg.Register(&registry.Node{ID: "worker1", DirectOK: true, CPUCores: 4, Models: []string{"m1"}, RpcAddr: "10.0.0.1:50052"})
	// Worker without model (should be excluded)
	reg.Register(&registry.Node{ID: "worker2", DirectOK: true, CPUCores: 4, Models: []string{"m2"}, RpcAddr: "10.0.0.2:50052"})

	asg, err := DistributedGroup{MinWorkers: 1}.Place(reg, false, Request{ModelID: "m1"})
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if asg.Kind != "distributed" {
		t.Fatalf("Kind = %s, want distributed", asg.Kind)
	}
	if asg.NodeID != "coord" {
		t.Fatalf("coordinator = %s, want coord", asg.NodeID)
	}
	if len(asg.Cohort) != 1 || asg.Cohort[0].NodeID != "worker1" {
		t.Fatalf("cohort = %+v, want [worker1]", asg.Cohort)
	}
	if asg.Cohort[0].RpcAddr != "10.0.0.1:50052" {
		t.Fatalf("RpcAddr = %s", asg.Cohort[0].RpcAddr)
	}
}

func TestDistributedGroupNoWorkers(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.Node{ID: "coord", DirectOK: true, CPUCores: 4, Models: []string{"m1"}})
	_, err := DistributedGroup{MinWorkers: 1}.Place(reg, false, Request{ModelID: "m1"})
	if err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}
