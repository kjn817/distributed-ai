package chunks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

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

func writeTestFile(t *testing.T, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "model.bin")
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fetchChunk(t *testing.T, dialer host.Host, srv host.Host, modelID string, index int) jobprotocol.ChunkResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ai := peer.AddrInfo{ID: srv.ID(), Addrs: srv.Addrs()}
	if err := dialer.Connect(ctx, ai); err != nil {
		t.Fatal(err)
	}
	s, err := dialer.NewStream(ctx, srv.ID(), jobprotocol.ChunkProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := json.NewEncoder(s).Encode(jobprotocol.ChunkRequest{ModelID: modelID, Index: index}); err != nil {
		t.Fatal(err)
	}
	_ = s.CloseWrite()
	var res jobprotocol.ChunkResponse
	if err := json.NewDecoder(s).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res
}

func TestServeChunkRoundTrip(t *testing.T) {
	srv := testHost(t)
	// 1.5 chunks: exercises a full chunk and a short tail chunk.
	path := writeTestFile(t, jobprotocol.ChunkSize+100)
	srv.SetStreamHandler(jobprotocol.ChunkProtocol,
		New(func(modelID string) (string, bool) {
			if modelID == "m1" {
				return path, true
			}
			return "", false
		}).HandleStream)

	dialer := testHost(t)
	res := fetchChunk(t, dialer, srv, "m1", 0)
	if res.Error != "" {
		t.Fatalf("chunk 0: %s", res.Error)
	}
	if len(res.Data) != jobprotocol.ChunkSize {
		t.Fatalf("chunk 0 size: got %d", len(res.Data))
	}
	for i, b := range res.Data {
		if b != byte(i%251) {
			t.Fatalf("chunk 0 data corrupt at %d", i)
		}
	}

	res = fetchChunk(t, dialer, srv, "m1", 1)
	if res.Error != "" {
		t.Fatalf("chunk 1: %s", res.Error)
	}
	if len(res.Data) != 100 {
		t.Fatalf("tail chunk size: got %d want 100", len(res.Data))
	}
}

func TestUnknownModel(t *testing.T) {
	srv := testHost(t)
	srv.SetStreamHandler(jobprotocol.ChunkProtocol,
		New(func(modelID string) (string, bool) { return "", false }).HandleStream)
	dialer := testHost(t)
	res := fetchChunk(t, dialer, srv, "nope", 0)
	if res.Error == "" {
		t.Fatal("expected error for unknown model")
	}
}

func TestChunkBeyondEOF(t *testing.T) {
	srv := testHost(t)
	path := writeTestFile(t, 100)
	srv.SetStreamHandler(jobprotocol.ChunkProtocol,
		New(func(modelID string) (string, bool) { return path, true }).HandleStream)
	dialer := testHost(t)
	res := fetchChunk(t, dialer, srv, "m1", 5)
	if res.Error == "" {
		t.Fatal("expected error for chunk beyond EOF")
	}
}
