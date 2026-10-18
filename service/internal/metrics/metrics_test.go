package metrics

import (
	"testing"
	"time"
)

func TestRecordAndRPS(t *testing.T) {
	tr := New()
	tr.Record("m1")
	tr.Record("m1")
	tr.Record("m2")
	if got := tr.RPS("m1"); got <= 0 {
		t.Fatalf("expected positive RPS, got %f", got)
	}
	// 2 requests in 5 minutes ≈ 0.0067 rps.
	if got := tr.RPS("m1"); got < 0.006 || got > 0.008 {
		t.Fatalf("RPS out of range: %f", got)
	}
	if got := tr.RPS("unknown"); got != 0 {
		t.Fatalf("expected 0 for unknown model, got %f", got)
	}
	snap := tr.Snapshot()
	byID := map[string]Stats{}
	for _, s := range snap {
		byID[s.ModelID] = s
	}
	if byID["m1"].RequestsTotal != 2 || byID["m2"].RequestsTotal != 1 {
		t.Fatalf("wrong totals: %+v", snap)
	}
}

func TestWindowPrunes(t *testing.T) {
	tr := New()
	tr.Record("m1")
	// Fake an old timestamp by reaching in (same package).
	tr.mu.Lock()
	tr.recent["m1"][0] = time.Now().Add(-10 * time.Minute)
	tr.mu.Unlock()
	if got := tr.RPS("m1"); got != 0 {
		t.Fatalf("old request should prune to 0 RPS, got %f", got)
	}
}
