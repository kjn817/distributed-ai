package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCandidateBackends(t *testing.T) {
	cases := []struct {
		p    Platform
		want []string
	}{
		{Platform{"darwin", "arm64", "apple"}, []string{"metal", "cpu"}},
		{Platform{"linux", "amd64", "nvidia"}, []string{"cuda", "vulkan", "cpu"}},
		{Platform{"linux", "amd64", "amd"}, []string{"rocm", "vulkan", "cpu"}},
		{Platform{"linux", "amd64", "unknown"}, []string{"vulkan", "cpu"}},
		{Platform{"windows", "amd64", "nvidia"}, []string{"cuda", "vulkan", "cpu"}},
		{Platform{"windows", "amd64", "unknown"}, []string{"vulkan", "cpu"}},
	}
	for _, c := range cases {
		got := CandidateBackends(c.p)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%v: got %v want %v", c.p, got, c.want)
		}
	}
}

// fakePackageServer serves engine tarballs: each contains a shell script
// named llama-server that exits 0 on --version.
func fakePackageServer(t *testing.T) *httptest.Server {
	t.Helper()
	buildTar := func() []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		script := []byte("#!/bin/sh\nexit 0\n")
		_ = tw.WriteHeader(&tar.Header{Name: "llama-server", Mode: 0o755, Size: int64(len(script))})
		_, _ = tw.Write(script)
		_ = tw.Close()
		_ = gz.Close()
		return buf.Bytes()
	}
	payload := buildTar()
	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > 7 && r.URL.Path[len(r.URL.Path)-7:] == ".sha256" {
			fmt.Fprint(w, hexSum+"  pkg.tar.gz\n") // filename field tolerated
			return
		}
		w.Write(payload)
	}))
}

func TestInstallVerifiesHash(t *testing.T) {
	srv := fakePackageServer(t)
	defer srv.Close()
	dir := t.TempDir()
	bin, err := Install(t.Context(), srv.URL, "linux", "amd64", "cpu", dir)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := filepath.Base(bin); got != "llama-server" {
		t.Fatalf("unexpected binary path %s", bin)
	}
	if !selfTest(t.Context(), bin) {
		t.Fatal("self-test should pass on the fake binary")
	}
}

func TestInstallRejectsTampered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > 7 && r.URL.Path[len(r.URL.Path)-7:] == ".sha256" {
			fmt.Fprint(w, "0000000000000000000000000000000000000000000000000000000000000000\n")
			return
		}
		w.Write([]byte("tampered-bytes"))
	}))
	defer srv.Close()
	if _, err := Install(t.Context(), srv.URL, "linux", "amd64", "cpu", t.TempDir()); err == nil {
		t.Fatal("expected hash mismatch error")
	}
}

func TestSelectBackendPicksFirstWorking(t *testing.T) {
	srv := fakePackageServer(t)
	defer srv.Close()
	backend, bin, err := SelectBackend(t.Context(), srv.URL, t.TempDir())
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if backend == "" || bin == "" {
		t.Fatal("expected backend and binary path")
	}
	// On this CI-less box the first candidate must win (all are fake-working).
	p := DetectPlatform()
	want := CandidateBackends(p)[0]
	if backend != want {
		t.Fatalf("expected first candidate %s, got %s", want, backend)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("binary missing: %v", err)
	}
}
