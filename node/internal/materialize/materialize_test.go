package materialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"

	"distributed-ai/node/internal/chunks"
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

func addrStrings(h host.Host) []string {
	var out []string
	for _, a := range h.Addrs() {
		out = append(out, a.String())
	}
	return out
}

func TestMaterializeRoundTrip(t *testing.T) {
	// Test "model": 1000 deterministic bytes → 1 chunk.
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	chunkSum := sha256.Sum256(data)
	fileSum := sha256.Sum256(data)
	chunkHex := hex.EncodeToString(chunkSum[:])
	fileHex := hex.EncodeToString(fileSum[:])

	holderH := testHost(t)
	modelPath := filepath.Join(t.TempDir(), "tiny.gguf")
	if err := os.WriteFile(modelPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	holderH.SetStreamHandler(jobprotocol.ChunkProtocol,
		chunks.New(func(modelID string) (string, bool) {
			if modelID == "tiny" {
				return modelPath, true
			}
			return "", false
		}).HandleStream)

	holderID := holderH.ID().String()
	holderAddrs := addrStrings(holderH)
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models/tiny/manifest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_id": "tiny", "size_bytes": 1000, "sha256": fileHex,
				"source": map[string]any{"file": "tiny.gguf"},
				"chunks": map[string]any{
					"size_bytes": jobprotocol.ChunkSize,
					"list": []map[string]any{
						{"index": 0, "size_bytes": 1000, "sha256": chunkHex},
					},
				},
			})
		case "/v1/models/tiny/holders":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"holders": []map[string]any{
					{"peer_id": holderID, "addrs": holderAddrs, "source_ip": "127.0.0.1"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer svc.Close()

	fetcher := testHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	destDir := t.TempDir()
	var gotChunks []string
	final, err := Materialize(ctx, fetcher, svc.URL, "tiny", destDir, func(peerID string, n int64) {
		gotChunks = append(gotChunks, peerID)
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotChunks) == 0 {
		t.Fatal("expected onChunk callbacks")
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size: got %d want %d", len(got), len(data))
	}
	for i := range got {
		if got[i] != data[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
	if filepath.Base(final) != "tiny.gguf" {
		t.Fatalf("final name: %s", final)
	}
}

func TestMaterializeNoHolders(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models/tiny/manifest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_id": "tiny", "size_bytes": 10, "sha256": "abc",
				"source": map[string]any{"file": "tiny.gguf"},
				"chunks": map[string]any{
					"size_bytes": jobprotocol.ChunkSize,
					"list":       []map[string]any{{"index": 0, "size_bytes": 10, "sha256": "abc"}},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"holders": []any{}})
	}))
	defer svc.Close()

	fetcher := testHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Materialize(ctx, fetcher, svc.URL, "tiny", t.TempDir(), nil, 0); err == nil {
		t.Fatal("expected error with no holders")
	}
}
