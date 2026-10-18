package replica

import (
	"os"
	"path/filepath"
	"testing"

	"distributed-ai/service/internal/metrics"
	"distributed-ai/service/internal/registry"
)

func TestPlanAssignsHealing(t *testing.T) {
	dir := t.TempDir()
	manifestYAML := `model_id: m1
version: 1
format: gguf
quant: q4_k_m
size_bytes: 1000
sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
source:
  type: seed
  repo: test
  file: m1.gguf
license: test
execution:
  strategies: ["FULL_MATERIALIZE"]
replication:
  min: 2
  target: 3
chunks:
  size_bytes: 512
  list:
    - {index: 0, size_bytes: 512, sha256: aaa}
    - {index: 1, size_bytes: 488, sha256: bbb}
`
	if err := os.WriteFile(filepath.Join(dir, "m1.yaml"), []byte(manifestYAML), 0644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.LoadModels(dir, dir); err != nil { // no file → PREPARING, manifest still loads
		t.Fatal(err)
	}
	// One holder (n1), one verified candidate with room (n2), one without (n3).
	reg.Register(&registry.Node{ID: "n1", CPUCores: 2, RAMBytes: 4e9, SSDSharedBytes: 10e9, DirectOK: true, Models: []string{"m1"}})
	reg.Register(&registry.Node{ID: "n2", CPUCores: 2, RAMBytes: 4e9, SSDSharedBytes: 10e9, DirectOK: true})
	reg.Register(&registry.Node{ID: "n3", CPUCores: 2, RAMBytes: 4e9, SSDSharedBytes: 10, DirectOK: true}) // too small
	tr := metrics.New()

	got := Plan(reg, tr)
	if len(got) != 1 || got[0].ModelID != "m1" || got[0].NodeID != "n2" {
		t.Fatalf("target 3, have 1, only n2 fits → want [{m1 n2}], got %v", got)
	}

	// After n2 becomes a holder, only... still need 1 more (target 3, have 2).
	// n3 can't fit; no more candidates → next plan assigns nothing new.
	reg.Register(&registry.Node{ID: "n2", CPUCores: 2, RAMBytes: 4e9, SSDSharedBytes: 10e9, DirectOK: true, Models: []string{"m1"}})
	got = Plan(reg, tr)
	if len(got) != 0 {
		t.Fatalf("no fitting candidates left: expected no assignments, got %v", got)
	}

	st := Statuses(reg, tr)
	if len(st) != 1 || st[0].Holders != 2 || st[0].Desired != 3 || st[0].Healthy {
		t.Fatalf("wrong status: %+v", st)
	}
}

func TestPlanNeedsManifest(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.Node{ID: "n1", CPUCores: 4, RAMBytes: 8e9, SSDSharedBytes: 100e9, DirectOK: true})
	tr := metrics.New()
	if got := Plan(reg, tr); len(got) != 0 {
		t.Fatalf("no models: expected no assignments, got %v", got)
	}
	if got := Statuses(reg, tr); len(got) != 0 {
		t.Fatalf("no models: expected no statuses, got %v", got)
	}
}

func TestDesiredDefaults(t *testing.T) {
	reg := registry.New()
	tr := metrics.New()
	if d := desiredHolders(reg, tr, "unknown"); d != 2 {
		t.Fatalf("expected default 2, got %d", d)
	}
	// Hot demand bumps the default.
	for i := 0; i < 40; i++ {
		tr.Record("unknown")
	}
	if d := desiredHolders(reg, tr, "unknown"); d != 3 {
		t.Fatalf("expected 3 under hot demand, got %d", d)
	}
}
