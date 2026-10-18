package model

import (
	"os"
	"path/filepath"
	"testing"
)

const realManifest = "../../../models/qwen2.5-0.5b-q4.yaml"
const realFile = "../../../models/qwen2.5-0.5b-instruct-q4_k_m.gguf"

func TestLoadRealManifest(t *testing.T) {
	if _, err := os.Stat(realManifest); os.IsNotExist(err) {
		t.Skip("manifest not present")
	}
	m, err := Load(realManifest)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.ModelID != "qwen2.5-0.5b-q4" {
		t.Fatalf("unexpected model_id %s", m.ModelID)
	}
	if m.Source.Repo == "" || m.License == "" {
		t.Fatal("manifest must pin source and license")
	}
	if len(m.Execution.Strategies) == 0 {
		t.Fatal("manifest must declare execution strategies")
	}
	if m.Execution.Strategies[0] != "FULL_MATERIALIZE" {
		t.Fatalf("v1 strategy must be FULL_MATERIALIZE, got %v", m.Execution.Strategies)
	}
}

func TestVerifyRealFile(t *testing.T) {
	if _, err := os.Stat(realFile); os.IsNotExist(err) {
		t.Skip("model file not present")
	}
	m, err := Load(realManifest)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := Verify(m, realFile); err != nil {
		t.Fatalf("verify real file: %v", err)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	m, err := Load(realManifest)
	if err != nil {
		t.Skip("manifest not present")
	}
	// Copy first 1MB and flip a byte: size differs AND hash differs.
	src, err := os.Open(realFile)
	if err != nil {
		t.Skip("model file not present")
	}
	defer src.Close()
	buf := make([]byte, 1<<20)
	n, _ := src.Read(buf)
	buf[0] ^= 0xff
	tmp := filepath.Join(t.TempDir(), "tampered.gguf")
	if err := os.WriteFile(tmp, buf[:n], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(m, tmp); err == nil {
		t.Fatal("expected verification failure for tampered file")
	}
}

func TestVerifyRejectsMissing(t *testing.T) {
	m := &Manifest{ModelID: "x", SizeBytes: 10, SHA256: "abc"}
	if err := Verify(m, "/nonexistent/file.gguf"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadDirMissingIsEmpty(t *testing.T) {
	got, err := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no manifests, got %d", len(got))
	}
}
