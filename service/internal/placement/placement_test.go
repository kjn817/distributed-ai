package placement

import (
	"errors"
	"testing"
	"time"

	"distributed-ai/service/internal/registry"
)

func testReg(nodes ...*registry.Node) *registry.Registry {
	reg := registry.New()
	for _, n := range nodes {
		reg.Register(n)
	}
	return reg
}

func TestRemoteFirst(t *testing.T) {
	reg := testReg(
		&registry.Node{ID: "small", CPUCores: 1, RAMBytes: 1e9, DirectOK: true},
		&registry.Node{ID: "big", CPUCores: 8, RAMBytes: 16e9, DirectOK: true},
	)
	a, err := SingleNodeFirst{}.Place(reg, true, Request{Strategy: StrategyFullMaterialize})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "remote" || a.NodeID != "big" {
		t.Fatalf("expected remote/big, got %+v", a)
	}
}

func TestLocalFallback(t *testing.T) {
	reg := testReg(
		&registry.Node{ID: "nat", CPUCores: 8, RAMBytes: 16e9}, // not verified
	)
	a, err := SingleNodeFirst{}.Place(reg, true, Request{Strategy: ""})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "local" {
		t.Fatalf("expected local fallback, got %+v", a)
	}
}

func TestNoCapacity(t *testing.T) {
	_, err := SingleNodeFirst{}.Place(testReg(), false, Request{})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}

func TestDistributedStrategyUnsupported(t *testing.T) {
	reg := testReg(&registry.Node{ID: "n", CPUCores: 8, DirectOK: true})
	_, err := SingleNodeFirst{}.Place(reg, true,
		Request{ModelID: "big-model", Strategy: StrategyDistributedMaterialize})
	if !errors.Is(err, ErrStrategyUnsupported) {
		t.Fatalf("expected ErrStrategyUnsupported, got %v", err)
	}
}

func TestRTTBreaksTies(t *testing.T) {
	now := time.Now()
	mk := func(id string, rtt int64) *registry.Node {
		return &registry.Node{ID: id, CPUCores: 4, RAMBytes: 8e9,
			DirectOK: true, RTTMs: rtt, LastHeartbeat: now}
	}
	reg := testReg(mk("slow", 250), mk("fast", 5))
	a, err := SingleNodeFirst{}.Place(reg, false, Request{Strategy: StrategyFullMaterialize})
	if err != nil {
		t.Fatal(err)
	}
	if a.NodeID != "fast" {
		t.Fatalf("expected low-RTT node to win, got %s", a.NodeID)
	}
}

func TestStaleHeartbeatPenalized(t *testing.T) {
	now := time.Now()
	reg := testReg(
		&registry.Node{ID: "stale", CPUCores: 8, RAMBytes: 16e9, DirectOK: true,
			LastHeartbeat: now.Add(-2 * time.Minute)},
		&registry.Node{ID: "fresh", CPUCores: 2, RAMBytes: 4e9, DirectOK: true,
			LastHeartbeat: now},
	)
	// stale: 8 + 8 - 5 = 11; fresh: 2 + 2 = 4 → stale still wins on raw power.
	a, err := SingleNodeFirst{}.Place(reg, false, Request{Strategy: StrategyFullMaterialize})
	if err != nil {
		t.Fatal(err)
	}
	if a.NodeID != "stale" {
		t.Fatalf("expected stale-but-powerful to win, got %s", a.NodeID)
	}
}

func TestInFlightSpreadsLoad(t *testing.T) {
	now := time.Now()
	mk := func(id string) *registry.Node {
		return &registry.Node{ID: id, CPUCores: 4, RAMBytes: 8e9,
			DirectOK: true, LastHeartbeat: now}
	}
	reg := testReg(mk("a"), mk("b"))
	// First request: tie → either (map order); force by inflight.
	reg.AddInFlight("a", 1) // a is busy
	got, err := SingleNodeFirst{}.Place(reg, false, Request{Strategy: StrategyFullMaterialize})
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "b" {
		t.Fatalf("expected idle node b, got %s", got.NodeID)
	}
	// Both busy equally → tie again (either is fine, must not error).
	reg.AddInFlight("b", 1)
	strategy := SingleNodeFirst{}
	if _, err = strategy.Place(reg, false, Request{Strategy: StrategyFullMaterialize}); err != nil {
		t.Fatal(err)
	}
	// Decrement floors at 0.
	reg.AddInFlight("a", -5)
	if n := reg.Get("a"); n.InFlight != 0 {
		t.Fatalf("in-flight must floor at 0, got %d", n.InFlight)
	}
}
