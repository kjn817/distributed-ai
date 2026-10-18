package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartMissingBinary(t *testing.T) {
	m := New(Config{ModelPath: "/tmp/x.gguf", Port: 18901})
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("expected error for missing binary path")
	}
}

func TestStartBadBinaryPath(t *testing.T) {
	m := New(Config{BinaryPath: "/nonexistent/llama-server", ModelPath: "/tmp/x.gguf", Port: 18902})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err == nil {
		_ = m.Stop()
		t.Fatal("expected error for bad binary path")
	}
}

func TestHealthOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	if !healthOK(srv.URL) {
		t.Fatal("expected healthOK true")
	}
}

func TestHealthNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"loading"}`))
	}))
	defer srv.Close()
	if healthOK(srv.URL) {
		t.Fatal("expected healthOK false while loading")
	}
	if healthOK("http://127.0.0.1:1") {
		t.Fatal("expected healthOK false for refused connection")
	}
}
