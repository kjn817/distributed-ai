package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteBaseURL(t *testing.T) {
	r := NewRemote("http://127.0.0.1:18082/")
	if got := r.BaseURL(); got != "http://127.0.0.1:18082" {
		t.Fatalf("trailing slash not trimmed: %q", got)
	}
	r = NewRemote("http://127.0.0.1:18082")
	if got := r.BaseURL(); got != "http://127.0.0.1:18082" {
		t.Fatalf("unexpected BaseURL: %q", got)
	}
}

func TestRemoteReady(t *testing.T) {
	// Healthy engine: /health returns 200.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ok.Close()

	r := NewRemote(ok.URL)
	if !r.Ready() {
		t.Fatal("expected Ready() true for healthy engine")
	}

	// Unhealthy engine: /health returns 500.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	r = NewRemote(bad.URL)
	if r.Ready() {
		t.Fatal("expected Ready() false for unhealthy engine")
	}

	// Down engine: connection refused.
	r = NewRemote("http://127.0.0.1:1")
	if r.Ready() {
		t.Fatal("expected Ready() false for unreachable engine")
	}
}

func TestRemoteSatisfiesInterface(t *testing.T) {
	var _ Engine = NewRemote("http://127.0.0.1:18082")
}
