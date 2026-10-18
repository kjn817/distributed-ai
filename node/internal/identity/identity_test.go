package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func withTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestLoadOrCreatePersists(t *testing.T) {
	withTempHome(t)
	k1, err := LoadOrCreate()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	k2, err := LoadOrCreate()
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !k1.Equals(k2) {
		t.Fatal("expected same key across restarts (Peer ID must persist)")
	}
}

func TestKeyFilePermissions(t *testing.T) {
	withTempHome(t)
	if _, err := LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(os.Getenv("HOME"), ".distributed-ai-node", "identity.key")
	fi, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file must be 0600, got %o", fi.Mode().Perm())
	}
}

func TestCorruptKeyFails(t *testing.T) {
	withTempHome(t)
	dir := filepath.Join(os.Getenv("HOME"), ".distributed-ai-node")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.key"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(); err == nil {
		t.Fatal("expected error for corrupt key file")
	}
}
