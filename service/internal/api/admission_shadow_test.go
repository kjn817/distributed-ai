package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"distributed-ai/service/internal/placement"
	"distributed-ai/service/internal/registry"
)

// Shadow mode OFF (default): the experiment endpoint behaves as if the
// feature did not exist, and chat routing is byte-for-byte the old path.
func TestAdmissionShadowOffByDefault(t *testing.T) {
	s := New(registry.New()).WithEngine(&fakeEngine{url: "http://127.0.0.1:1", ready: true})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/experiments/admission/shadow", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("shadow endpoint: expected 404 when off, got %d", rec.Code)
	}
}

// Shadow mode ON: a chat request that gets placed records exactly one
// decision, and the response is the same as without shadow mode.
// With no measurement sources in the registry, the recorded decision
// must honestly be insufficient_data.
func TestAdmissionShadowRecordsWithoutChangingRouting(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[]}`))
	}))
	defer backend.Close()

	build := func() (*Server, *placement.ShadowRecorder) {
		rec := placement.NewShadowRecorder(8)
		s := New(registry.New()).
			WithEngine(&fakeEngine{url: backend.URL, ready: true}).
			WithAdmissionShadow(rec, 10, "test-admin-key")
		return s, rec
	}
	chat := func(s *Server) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"qwen2.5-0.5b-q4","messages":[]}`))
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}

	on, rec := build()
	respOn := chat(on)
	if respOn.Code != http.StatusOK {
		t.Fatalf("shadow on: expected 200, got %d: %s", respOn.Code, respOn.Body.String())
	}
	if rec.Len() != 1 {
		t.Fatalf("shadow on: expected 1 recorded decision, got %d", rec.Len())
	}
	d := rec.Snapshot()[0].Decision
	if d.Mode != placement.ModeInsufficientData {
		t.Fatalf("recorded mode = %s, want insufficient_data (no measurement sources exist yet)", d.Mode)
	}
	if !d.Shadow {
		t.Fatal("recorded decision must be marked shadow")
	}

	// Same request with shadow OFF reaches the same response.
	off := New(registry.New()).WithEngine(&fakeEngine{url: backend.URL, ready: true})
	respOff := chat(off)
	if respOff.Code != respOn.Code || respOff.Body.String() != respOn.Body.String() {
		t.Fatalf("shadow mode changed routing: on=%d %s off=%d %s",
			respOn.Code, respOn.Body.String(), respOff.Code, respOff.Body.String())
	}

	// The endpoint serves the recorded decisions as JSON — but only to
	// the admin key holder.
	unauth := httptest.NewRecorder()
	on.Routes().ServeHTTP(unauth, httptest.NewRequest("GET", "/v1/experiments/admission/shadow", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("shadow endpoint without key: expected 401, got %d", unauth.Code)
	}
	rec2 := httptest.NewRecorder()
	areq := httptest.NewRequest("GET", "/v1/experiments/admission/shadow", nil)
	areq.Header.Set("Authorization", "Bearer test-admin-key")
	on.Routes().ServeHTTP(rec2, areq)
	if rec2.Code != http.StatusOK {
		t.Fatalf("shadow endpoint: expected 200, got %d", rec2.Code)
	}
	// The payload must not leak request content or network details:
	// none of these fields may appear anywhere in the response.
	for _, banned := range []string{`"messages"`, `"content"`, `"prompt"`, `"api_key"`, `"source_ip"`, `"addrs"`, `"rpc_addr"`} {
		if strings.Contains(rec2.Body.String(), banned) {
			t.Fatalf("shadow payload leaks %s: %s", banned, rec2.Body.String())
		}
	}
	var payload struct {
		Enabled   bool `json:"enabled"`
		Decisions []struct {
			Decision struct {
				Mode string `json:"mode"`
			} `json:"decision"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &payload); err != nil {
		t.Fatalf("shadow endpoint JSON: %v", err)
	}
	if !payload.Enabled || len(payload.Decisions) != 1 ||
		payload.Decisions[0].Decision.Mode != placement.ModeInsufficientData {
		t.Fatalf("unexpected shadow payload: %s", rec2.Body.String())
	}
}

// Access control matrix for the shadow endpoint: wrong key is rejected,
// and shadow ON without a configured admin key keeps the endpoint closed
// even for requests that present some key.
func TestAdmissionShadowAuthMatrix(t *testing.T) {
	rec := placement.NewShadowRecorder(8)
	withKey := New(registry.New()).WithAdmissionShadow(rec, 10, "secret-key")
	get := func(s *Server, auth string) int {
		req := httptest.NewRequest("GET", "/v1/experiments/admission/shadow", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}
	if c := get(withKey, ""); c != http.StatusUnauthorized {
		t.Errorf("no header: %d, want 401", c)
	}
	if c := get(withKey, "Bearer wrong"); c != http.StatusUnauthorized {
		t.Errorf("wrong key: %d, want 401", c)
	}
	if c := get(withKey, "secret-key"); c != http.StatusUnauthorized {
		t.Errorf("missing Bearer prefix handling must not accept raw key: %d, want 401", c)
	}
	if c := get(withKey, "Bearer secret-key"); c != http.StatusOK {
		t.Errorf("correct key: %d, want 200", c)
	}

	noKey := New(registry.New()).WithAdmissionShadow(placement.NewShadowRecorder(8), 10, "")
	if c := get(noKey, "Bearer anything"); c != http.StatusForbidden {
		t.Errorf("no admin key configured: %d, want 403 (endpoint closed)", c)
	}
}

// A failed placement (no capacity) records nothing: shadow observation
// only covers requests that were actually placed by the existing policy.
func TestAdmissionShadowSkipsUnplacedRequests(t *testing.T) {
	rec := placement.NewShadowRecorder(8)
	s := New(registry.New()).WithAdmissionShadow(rec, 10, "k")
	resp := httptest.NewRecorder()
	s.Routes().ServeHTTP(resp, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`)))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.Code)
	}
	if rec.Len() != 0 {
		t.Fatalf("expected no records for unplaced request, got %d", rec.Len())
	}
}
