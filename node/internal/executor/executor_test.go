package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"distributed-ai/engine"
	jobprotocol "distributed-ai/protocol"
)

func testHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// roundTrip opens a real libp2p stream to a host serving ex and returns the
// decoded JobResult.
func roundTrip(t *testing.T, srv host.Host, ex *Executor, req jobprotocol.JobRequest) jobprotocol.JobResult {
	t.Helper()
	srv.SetStreamHandler(jobprotocol.JobProtocol, ex.HandleStream)

	dialer := testHost(t)
	ai := peer.AddrInfo{ID: srv.ID(), Addrs: srv.Addrs()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dialer.Connect(ctx, ai); err != nil {
		t.Fatal(err)
	}
	s, err := dialer.NewStream(ctx, srv.ID(), jobprotocol.JobProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := json.NewEncoder(s).Encode(req); err != nil {
		t.Fatal(err)
	}
	_ = s.CloseWrite()
	var res jobprotocol.JobResult
	if err := json.NewDecoder(s).Decode(&res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return res
}

func testExecutor() *Executor {
	mgr := engine.New(engine.Config{BinaryPath: "/nonexistent", ModelPath: "/nonexistent", Port: 19999})
	return New(mgr, "test-model", engine.Config{})
}

func TestWrongModelRejected(t *testing.T) {
	srv := testHost(t)
	res := roundTrip(t, srv, testExecutor(), jobprotocol.JobRequest{
		JobID: "j1", ModelID: "other-model", Strategy: "FULL_MATERIALIZE",
		Messages: []jobprotocol.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if res.Error == "" {
		t.Fatal("expected error for wrong model")
	}
	if res.JobID != "j1" {
		t.Fatalf("job id not echoed: %q", res.JobID)
	}
}

func TestBadStrategyRejected(t *testing.T) {
	srv := testHost(t)
	res := roundTrip(t, srv, testExecutor(), jobprotocol.JobRequest{
		JobID: "j2", ModelID: "test-model", Strategy: "DISTRIBUTED_MATERIALIZE",
	})
	if res.Error == "" {
		t.Fatal("expected error for unsupported strategy")
	}
}

func TestEngineNotReadyRejected(t *testing.T) {
	srv := testHost(t)
	res := roundTrip(t, srv, testExecutor(), jobprotocol.JobRequest{
		JobID: "j3", ModelID: "test-model", Strategy: "FULL_MATERIALIZE",
		Messages: []jobprotocol.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if res.Error == "" {
		t.Fatal("expected error when engine not started")
	}
}
