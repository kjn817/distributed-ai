package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"

	jobprotocol "distributed-ai/protocol"
	"distributed-ai/service/internal/registry"
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

func addrStrings(h host.Host) []string {
	var out []string
	for _, a := range h.Addrs() {
		out = append(out, a.String())
	}
	return out
}

func TestDispatchRoundTrip(t *testing.T) {
	svc := testHost(t)
	nodeH := testHost(t)

	// Stub worker: real stream, canned real-shaped result.
	nodeH.SetStreamHandler(jobprotocol.JobProtocol, func(s network.Stream) {
		defer s.Close()
		var req jobprotocol.JobRequest
		if err := json.NewDecoder(s).Decode(&req); err != nil {
			return
		}
		_ = json.NewEncoder(s).Encode(jobprotocol.JobResult{
			JobID: req.JobID, Content: "hello from worker",
			PromptTokens: 10, GenTokens: 3, DurationMs: 42,
		})
	})

	node := &registry.Node{ID: nodeH.ID().String(), Addrs: addrStrings(nodeH), SourceIP: "127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Dispatch(ctx, svc, node, &jobprotocol.JobRequest{
		JobID: "j1", ModelID: "m", Strategy: "FULL_MATERIALIZE",
		Messages:  []jobprotocol.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hello from worker" || res.JobID != "j1" {
		t.Fatalf("bad result: %+v", res)
	}
}

func TestDispatchRemoteError(t *testing.T) {
	svc := testHost(t)
	nodeH := testHost(t)
	nodeH.SetStreamHandler(jobprotocol.JobProtocol, func(s network.Stream) {
		defer s.Close()
		var req jobprotocol.JobRequest
		_ = json.NewDecoder(s).Decode(&req)
		_ = json.NewEncoder(s).Encode(jobprotocol.JobResult{JobID: req.JobID, Error: "boom"})
	})
	node := &registry.Node{ID: nodeH.ID().String(), Addrs: addrStrings(nodeH), SourceIP: "127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := Dispatch(ctx, svc, node, &jobprotocol.JobRequest{JobID: "j2"}); err == nil {
		t.Fatal("expected remote error to surface")
	}
}

func TestDispatchUnreachable(t *testing.T) {
	svc := testHost(t)
	ghost := testHost(t) // real ID, but we'll only offer an unroutable addr
	node := &registry.Node{ID: ghost.ID().String(), Addrs: []string{"/ip4/192.0.2.1/tcp/4001"}, SourceIP: ""}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := Dispatch(ctx, svc, node, &jobprotocol.JobRequest{JobID: "j3"}); err == nil {
		t.Fatal("expected connect failure")
	}
}
