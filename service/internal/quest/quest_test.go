package quest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"distributed-ai/service/internal/registry"
)

func bigRegistry() *registry.Registry {
	reg := registry.New()
	// Two verified compute nodes, plenty of resources.
	reg.Register(&registry.Node{ID: "n1", CPUCores: 8, RAMBytes: 32e9, SSDSharedBytes: 500e9, DirectOK: true})
	reg.Register(&registry.Node{ID: "n2", CPUCores: 8, RAMBytes: 32e9, SSDSharedBytes: 500e9, DirectOK: true})
	reg.Register(&registry.Node{ID: "n3", CPUCores: 4, RAMBytes: 16e9, SSDSharedBytes: 200e9}) // unverified
	return reg
}

func TestEmptyNetworkAllLocked(t *testing.T) {
	entries := Evaluate(registry.New())
	if len(entries) != len(Ladder) {
		t.Fatalf("expected %d entries, got %d", len(Ladder), len(entries))
	}
	for _, e := range entries {
		if e.State != "LOCKED" {
			t.Errorf("%s: expected LOCKED, got %s", e.ModelID, e.State)
		}
		if e.Progress != 0 {
			t.Errorf("%s: expected 0 progress, got %f", e.ModelID, e.Progress)
		}
	}
}

func TestBigNetworkUnlocksSmallModels(t *testing.T) {
	entries := Evaluate(bigRegistry())
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ModelID] = e
	}
	// 0.5B..3B requirements are met by this network.
	for _, id := range []string{"qwen2.5-0.5b-q4", "qwen2.5-1.5b-q4", "qwen2.5-3b-q4"} {
		if byID[id].State != "ELIGIBLE" {
			t.Errorf("%s: expected ELIGIBLE, got %s (progress %f)", id, byID[id].State, byID[id].Progress)
		}
	}
	// 7B needs 3 compute nodes; we have 2.
	if byID["qwen2.5-7b-q4"].State != "LOCKED" {
		t.Errorf("7b: expected LOCKED, got %s", byID["qwen2.5-7b-q4"].State)
	}
}

func TestProgressIsBottleneck(t *testing.T) {
	reg := registry.New()
	// Plenty of storage/RAM/nodes, but only 1 verified compute node.
	// 1.5B needs 2 → progress must be 0.5, bottlenecked on compute.
	reg.Register(&registry.Node{ID: "n1", CPUCores: 64, RAMBytes: 1e12, SSDSharedBytes: 1e12, DirectOK: true})
	reg.Register(&registry.Node{ID: "n2", CPUCores: 64, RAMBytes: 1e12, SSDSharedBytes: 1e12, DirectOK: true})
	reg.Register(&registry.Node{ID: "n3", CPUCores: 64, RAMBytes: 1e12, SSDSharedBytes: 1e12, DirectOK: true})
	entries := Evaluate(reg)
	var e15 *Entry
	for i := range entries {
		if entries[i].ModelID == "qwen2.5-1.5b-q4" {
			e15 = &entries[i]
		}
	}
	if e15 == nil {
		t.Fatal("1.5b entry missing")
	}
	// Make only 1 node verified: re-register n2,n3 without DirectOK.
	reg2 := registry.New()
	reg2.Register(&registry.Node{ID: "n1", CPUCores: 64, RAMBytes: 1e12, SSDSharedBytes: 1e12, DirectOK: true})
	reg2.Register(&registry.Node{ID: "n2", CPUCores: 64, RAMBytes: 1e12, SSDSharedBytes: 1e12})
	reg2.Register(&registry.Node{ID: "n3", CPUCores: 64, RAMBytes: 1e12, SSDSharedBytes: 1e12})
	entries2 := Evaluate(reg2)
	for i := range entries2 {
		if entries2[i].ModelID == "qwen2.5-1.5b-q4" {
			if entries2[i].Progress != 0.5 {
				t.Fatalf("expected bottleneck progress 0.5, got %f", entries2[i].Progress)
			}
			if entries2[i].State != "LOCKED" {
				t.Fatalf("expected LOCKED, got %s", entries2[i].State)
			}
		}
	}
}

func TestActiveOverridesEligible(t *testing.T) {
	// A verified model file is ACTIVE even with zero network resources.
	dir := t.TempDir()
	content := []byte("tiny-model-bytes")
	if err := os.WriteFile(filepath.Join(dir, "tiny.gguf"), content, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	manifestYAML := `model_id: qwen2.5-0.5b-q4
version: 1
format: gguf
quant: q4_k_m
size_bytes: 16
sha256: ` + hex.EncodeToString(sum[:]) + `
source:
  type: local
  repo: test
  file: tiny.gguf
license: test
execution:
  strategies: ["FULL_MATERIALIZE"]
`
	if err := os.WriteFile(filepath.Join(dir, "m.yaml"), []byte(manifestYAML), 0644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.LoadModels(dir, dir); err != nil {
		t.Fatal(err)
	}
	entries := Evaluate(reg)
	for _, e := range entries {
		if e.ModelID == "qwen2.5-0.5b-q4" {
			if e.State != "ACTIVE" {
				t.Fatalf("expected ACTIVE, got %s", e.State)
			}
			if e.Progress != 1 {
				t.Fatalf("expected progress 1, got %f", e.Progress)
			}
			return
		}
	}
	t.Fatal("0.5b entry missing")
}
