// Package metrics tracks per-model inference demand from real requests.
//
// The replica planner (M6b) uses these numbers to decide desired replica
// counts. Nothing is estimated: every data point is a request the service
// actually served.
package metrics

import (
	"sync"
	"time"
)

// window is how far back the RPS rate looks.
const window = 5 * time.Minute

// Stats is one model's demand snapshot.
type Stats struct {
	ModelID       string  `json:"model_id"`
	RequestsTotal int64   `json:"requests_total"`
	RPS           float64 `json:"rps_5m"`
}

// Tracker counts requests per model.
type Tracker struct {
	mu     sync.Mutex
	total  map[string]int64
	recent map[string][]time.Time // request timestamps within window
}

// New creates an empty Tracker.
func New() *Tracker {
	return &Tracker{total: map[string]int64{}, recent: map[string][]time.Time{}}
}

// Record logs one served request for modelID.
func (t *Tracker) Record(modelID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total[modelID]++
	now := time.Now()
	t.recent[modelID] = append(t.recent[modelID], now)
}

// prune drops timestamps older than the window; caller holds the lock.
func (t *Tracker) prune(modelID string, now time.Time) {
	cutoff := now.Add(-window)
	ts := t.recent[modelID]
	keep := ts[:0]
	for _, x := range ts {
		if x.After(cutoff) {
			keep = append(keep, x)
		}
	}
	// Clear the tail so pruned times don't leak.
	for i := len(keep); i < len(ts); i++ {
		ts[i] = time.Time{}
	}
	t.recent[modelID] = keep
}

// RPS returns the requests-per-second for modelID over the window.
func (t *Tracker) RPS(modelID string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(modelID, time.Now())
	return float64(len(t.recent[modelID])) / window.Seconds()
}

// Snapshot returns demand stats for every model seen.
func (t *Tracker) Snapshot() []Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	out := make([]Stats, 0, len(t.total))
	for id, total := range t.total {
		t.prune(id, now)
		out = append(out, Stats{
			ModelID:       id,
			RequestsTotal: total,
			RPS:           float64(len(t.recent[id])) / window.Seconds(),
		})
	}
	return out
}
