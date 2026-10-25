package main

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	testPeerA = "12D3KooWD8TMrQrse4oVQUvooR6rJoU96DiwexpsyohYSPEstgqV"
	testPeerB = "12D3KooWGKTkRcQZx4cagU1zyDMLerLupYWRVRD5XDCmAiuSjk1u"
)

func TestParseAllowlistIDs(t *testing.T) {
	ids, err := parseAllowlistIDs(testPeerA + ", " + testPeerB + ",")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0].String() != testPeerA || ids[1].String() != testPeerB {
		t.Fatalf("parsed = %v, want [%s %s]", ids, testPeerA, testPeerB)
	}
	if _, err := parseAllowlistIDs("not-a-peer-id"); err == nil {
		t.Fatal("bad peer id was accepted")
	}
	if ids, err := parseAllowlistIDs(""); err != nil || len(ids) != 0 {
		t.Fatalf("empty input: ids=%v err=%v", ids, err)
	}
}

func TestLoadAllowlistFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allow.txt")
	content := "# members\n" + testPeerA + "\n\n" + testPeerB + " # node-b\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := loadAllowlistFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0].String() != testPeerA || ids[1].String() != testPeerB {
		t.Fatalf("loaded = %v, want [%s %s]", ids, testPeerA, testPeerB)
	}
	// Revocation surface: removing a line removes the member. A file
	// with a bad line must fail loudly with its line number, never
	// silently skip the entry.
	badPath := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(badPath, []byte(testPeerA+"\nnope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAllowlistFile(badPath); err == nil {
		t.Fatal("bad peer id line was accepted")
	} else {
		t.Logf("bad line rejected as expected: %v", err)
	}
	if _, err := loadAllowlistFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Fatal("missing allow file was accepted")
	}
}
