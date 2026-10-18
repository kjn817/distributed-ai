package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"distributed-ai/engine"
	"distributed-ai/service/internal/registry"
)

// fakeEngine implements engine.Engine against an httptest server.
type fakeEngine struct {
	url   string
	ready bool
}

func (f *fakeEngine) Ready() bool     { return f.ready }
func (f *fakeEngine) BaseURL() string { return f.url }

var _ engine.Engine = (*fakeEngine)(nil)

func TestChatCompletionsNoEngine(t *testing.T) {
	s := New(registry.New())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_capacity") {
		t.Fatalf("expected no_capacity, got %s", rec.Body.String())
	}
}

func TestChatCompletionsProxied(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected proxied path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[]}`))
	}))
	defer backend.Close()

	s := New(registry.New()).WithEngine(&fakeEngine{url: backend.URL, ready: true})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen2.5-0.5b-q4","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "chatcmpl-test") {
		t.Fatalf("expected proxied body, got %s", rec.Body.String())
	}
}

func TestChatCompletionsEngineNotReady(t *testing.T) {
	s := New(registry.New()).WithEngine(&fakeEngine{url: "http://127.0.0.1:1", ready: false})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestNodeLifecycle(t *testing.T) {
	s := New(registry.New())
	h := s.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/nodes/register",
		strings.NewReader(`{"id":"n1","os":"linux","cpu_cores":4,"direct_ok":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("register: expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/nodes/heartbeat",
		strings.NewReader(`{"node_id":"n1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/network/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"online_nodes":1`) {
		t.Fatalf("expected 1 online node, got %s", rec.Body.String())
	}
}
