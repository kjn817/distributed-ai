package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-ai/node/internal/protocol"
)

func sampleInfo() protocol.ResourceInfo {
	return protocol.ResourceInfo{
		NodeName: "n1", PeerID: "peer-abc", OS: "linux", Architecture: "amd64",
		CPUModel: "Test CPU", PhysicalCores: 4, LogicalCores: 8, RAMTotalGB: 16,
		GPUModel: "Test GPU", GPUVendor: "NVIDIA",
		StorageTotalGB: 100, StorageUsedGB: 30, StorageFreeGB: 70,
		CPUShare: 50, GPUShare: 25, RAMShare: 25, StorageShare: 50,
	}
}

func TestMapAppliesShares(t *testing.T) {
	p := Map(sampleInfo(), []string{"/ip4/127.0.0.1/tcp/4001"}, []string{"m1"}, "")
	if p.ID != "peer-abc" || p.OS != "linux" || p.CPUModel != "Test CPU" {
		t.Fatalf("identity fields wrong: %+v", p)
	}
	if p.CPUCores != 4 { // 8 logical × 50%
		t.Fatalf("cpu_cores: got %d want 4", p.CPUCores)
	}
	if want := int64(4 * gb); p.RAMBytes != want { // 16GB × 25%
		t.Fatalf("ram_bytes: got %d want %d", p.RAMBytes, want)
	}
	if want := int64(35 * gb); p.SSDSharedBytes != want { // 70GB free × 50%
		t.Fatalf("ssd_shared_bytes: got %d want %d", p.SSDSharedBytes, want)
	}
	if p.DirectOK {
		t.Fatal("direct_ok must be false: unverified, service dials back in M3")
	}
}

func TestMapUnknownGPUNotCounted(t *testing.T) {
	info := sampleInfo()
	info.GPUModel = "Unknown"
	if p := Map(info, nil, nil, ""); p.GPUModel != "" {
		t.Fatalf("Unknown GPU must map to empty, got %q", p.GPUModel)
	}
}

func TestMapCarriesModels(t *testing.T) {
	p := Map(sampleInfo(), nil, []string{"m1", "m2"}, "127.0.0.1:50052")
	if len(p.Models) != 2 || p.Models[0] != "m1" {
		t.Fatalf("models not carried: %+v", p.Models)
	}
}

func TestMapCarriesAddrs(t *testing.T) {
	addrs := []string{"/ip4/127.0.0.1/tcp/4001"}
	p := Map(sampleInfo(), addrs, nil, "")
	if len(p.Addrs) != 1 || p.Addrs[0] != addrs[0] {
		t.Fatalf("addrs not carried: %+v", p.Addrs)
	}
}

func TestMapZeroShare(t *testing.T) {
	info := sampleInfo()
	info.CPUShare = 0
	if p := Map(info, nil, nil, ""); p.CPUCores != 0 {
		t.Fatalf("zero share must map to zero, got %d", p.CPUCores)
	}
}

func TestRegisterPostsPayload(t *testing.T) {
	var got nodePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/nodes/register" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if err := Register(context.Background(), srv.URL, sampleInfo(), nil, nil, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got.ID != "peer-abc" || got.CPUCores != 4 {
		t.Fatalf("payload wrong: %+v", got)
	}
}

func TestRegisterFailsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := Register(context.Background(), srv.URL, sampleInfo(), nil, nil, ""); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestRunHeartbeats(t *testing.T) {
	var heartbeats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/nodes/heartbeat" {
			var body struct {
				NodeID string `json:"node_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.NodeID != "peer-abc" {
				t.Errorf("wrong node_id %q", body.NodeID)
			}
			heartbeats.Add(1)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	var errs atomic.Int32
	Run(ctx, Config{
		APIURL: srv.URL, Info: sampleInfo(), Models: func() []string { return nil },
		Interval: 100 * time.Millisecond, OnErr: func(err error) { errs.Add(1) },
	})
	if n := heartbeats.Load(); n < 2 {
		t.Fatalf("expected ≥2 heartbeats, got %d", n)
	}
	if errs.Load() != 0 {
		t.Fatalf("unexpected errors: %d", errs.Load())
	}
}

func TestRunStopsOnRegisterFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	called := make(chan error, 1)
	Run(ctx, Config{
		APIURL: srv.URL, Info: sampleInfo(), Models: func() []string { return nil },
		Interval: 50 * time.Millisecond, OnErr: func(err error) { called <- err },
	})
	select {
	case err := <-called:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(time.Second):
		t.Fatal("onErr not called on register failure")
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&statusError{op: "heartbeat", code: 404}) {
		t.Fatal("expected 404 to be not-found")
	}
	if IsNotFound(&statusError{op: "heartbeat", code: 500}) {
		t.Fatal("500 must not be not-found")
	}
	if IsNotFound(errors.New("boom")) {
		t.Fatal("plain error must not be not-found")
	}
	if IsNotFound(nil) {
		t.Fatal("nil must not be not-found")
	}
}

// TestRunReregistersOnHeartbeat404 simulates a service restart: the first
// heartbeat 404s, the node must re-register (with the current models list)
// instead of heartbeating into the void.
func TestRunReregistersOnHeartbeat404(t *testing.T) {
	var mu sync.Mutex
	var registers, heartbeats int
	var lastModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/nodes/register":
			registers++
			var p nodePayload
			_ = json.NewDecoder(r.Body).Decode(&p)
			lastModels = p.Models
			w.Write([]byte(`{"ok":true}`))
		case "/v1/nodes/heartbeat":
			heartbeats++
			if heartbeats == 1 {
				w.WriteHeader(http.StatusNotFound) // service forgot us
				return
			}
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	models := []string{"m1"}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var mu2 sync.Mutex
	var events []string
	Run(ctx, Config{
		APIURL: srv.URL, Info: sampleInfo(), Models: func() []string { return models },
		Interval: 100 * time.Millisecond,
		OnErr: func(err error) {
			mu2.Lock()
			events = append(events, err.Error())
			mu2.Unlock()
		},
	})
	// After the 404 the node re-registers; then it adds a model and the
	// NEXT re-register (if any) would carry it. Here we assert the
	// re-register happened with the models list from the func.
	mu.Lock()
	defer mu.Unlock()
	if registers < 2 {
		t.Fatalf("expected re-register after 404, got %d registers", registers)
	}
	if len(lastModels) != 1 || lastModels[0] != "m1" {
		t.Fatalf("re-register must carry current models, got %v", lastModels)
	}
	mu2.Lock()
	defer mu2.Unlock()
	found := false
	for _, e := range events {
		if strings.Contains(e, "re-registered") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected re-registered event, got %v", events)
	}
}

func TestRunDeliversAssignments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/nodes/heartbeat" {
			var body struct {
				ChunkBytes int64 `json:"chunk_bytes_served"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ChunkBytes != 1234 {
				t.Errorf("expected chunk_bytes_served 1234, got %d", body.ChunkBytes)
			}
			w.Write([]byte(`{"ok":true,"replicate":["m1","m2"]}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	got := make(chan []string, 4)
	Run(ctx, Config{
		APIURL: srv.URL, Info: sampleInfo(), Models: func() []string { return nil },
		ChunkBytes: func() int64 { return 1234 },
		Interval:   100 * time.Millisecond,
		OnErr: func(err error) {
			// Test teardown cancels in-flight heartbeats; not a code bug.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			t.Errorf("unexpected error: %v", err)
		},
		OnAssign: func(ids []string) { got <- ids },
	})
	select {
	case ids := <-got:
		if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
			t.Fatalf("wrong assignments: %v", ids)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAssign not called")
	}
}

func TestHeartbeatReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := Config{APIURL: srv.URL, Info: sampleInfo()}
	_, err := Heartbeat(context.Background(), cfg)
	if !IsNotFound(err) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestSharedCores(t *testing.T) {
	if got := SharedCores(8, 50); got != 4 {
		t.Fatalf("expected 4, got %d", got)
	}
	if got := SharedCores(8, 0); got != 1 {
		t.Fatalf("expected floor of 1, got %d", got)
	}
	if got := SharedCores(2, 10); got != 1 {
		t.Fatalf("expected 1 (0.2 rounds to 0, floored to 1), got %d", got)
	}
}
