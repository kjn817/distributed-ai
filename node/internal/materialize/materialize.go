// Package materialize fetches a model over P2P chunk transfer and assembles
// it into a verified local file.
//
// Flow: manifest from the service (chunk hashes are the trust root) → chunk
// holders from the service → download each chunk over /distributed-ai/chunk/1
// (round-robin over holders, per-chunk SHA-256 verified) → assemble → verify
// whole-file size + SHA-256 → move into place. Any verification failure aborts
// honestly: a model that doesn't verify is never served.
package materialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	jobprotocol "distributed-ai/protocol"
)

// chunkInfo mirrors the manifest's chunk pinning (only what we need).
type chunkInfo struct {
	Index     int    `json:"index"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// manifest mirrors the service's manifest JSON (only what we need).
type manifest struct {
	ModelID   string `json:"model_id"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Source    struct {
		File string `json:"file"`
	} `json:"source"`
	Chunks struct {
		SizeBytes int64       `json:"size_bytes"`
		List      []chunkInfo `json:"list"`
	} `json:"chunks"`
}

// holder is one peer that holds the model.
type holder struct {
	PeerID   string   `json:"peer_id"`
	Addrs    []string `json:"addrs"`
	SourceIP string   `json:"source_ip"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func getJSON(url string, v any) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("materialize: GET %s: http %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Materialize assembles modelID into destDir and returns the final file path.
// Materialize fetches a model over P2P chunk transfer, verifies every chunk
// and the assembled file, and returns the final path. onChunk (may be nil)
// is called per chunk with the serving peer's ID and byte count — the
// receiver-confirmed basis for transfer credit (not the sender's claim).
// maxBytes caps the models directory usage (SSD share enforcement);
// 0 = no cap.
func Materialize(ctx context.Context, h host.Host, serviceURL, modelID, destDir string, onChunk func(peerID string, bytes int64), maxBytes int64) (string, error) {
	var mf manifest
	if err := getJSON(serviceURL+"/v1/models/"+modelID+"/manifest", &mf); err != nil {
		return "", err
	}
	if len(mf.Chunks.List) == 0 {
		return "", fmt.Errorf("materialize: manifest for %q has no chunks", modelID)
	}
	// SSD share enforcement: refuse when the model wouldn't fit the budget.
	if maxBytes > 0 {
		used, err := dirSize(destDir)
		if err == nil && used+mf.SizeBytes > maxBytes {
			return "", fmt.Errorf("materialize: SSD share exceeded (used %d + model %d > budget %d)",
				used, mf.SizeBytes, maxBytes)
		}
	}
	var holdersResp struct {
		Holders []holder `json:"holders"`
	}
	if err := getJSON(serviceURL+"/v1/models/"+modelID+"/holders", &holdersResp); err != nil {
		return "", err
	}
	if len(holdersResp.Holders) == 0 {
		return "", fmt.Errorf("materialize: no holders for %q", modelID)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", err
	}

	finalName := mf.Source.File
	if finalName == "" {
		finalName = modelID + ".gguf"
	}
	final := filepath.Join(destDir, finalName)
	// Idempotent: a valid file already in place needs no download.
	// (Lets replicate assignments and lazy jobs share one path.)
	if err := verifyFile(final, mf.SizeBytes, mf.SHA256); err == nil {
		return final, nil
	}

	tmp, err := os.CreateTemp(destDir, ".materialize-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	// Grow the file so writes at offsets work.
	if err := tmp.Truncate(mf.SizeBytes); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}

	for i, c := range mf.Chunks.List {
		srcPeer, err := fetchChunk(ctx, h, mf.ModelID, tmp, c, holdersResp.Holders, i)
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return "", fmt.Errorf("materialize: chunk %d: %w", c.Index, err)
		}
		if onChunk != nil {
			onChunk(srcPeer, c.SizeBytes)
		}
		_ = i
	}
	tmp.Close()

	// Verify the assembled file before it becomes servable.
	if err := verifyFile(tmpName, mf.SizeBytes, mf.SHA256); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("materialize: assembled file invalid: %w", err)
	}

	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return final, nil
}

// dirSize sums regular file sizes under dir (0 when dir is missing).
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// fetchChunk downloads one chunk (trying holders in rotation) and writes it
// at its offset after hash verification. Returns the peer ID that served it.
func fetchChunk(ctx context.Context, h host.Host, modelID string, f *os.File, c chunkInfo, holders []holder, pos int) (string, error) {
	var lastErr error
	for k := range holders {
		hd := holders[(pos+k)%len(holders)]
		data, err := downloadChunk(ctx, h, hd, modelID, c)
		if err != nil {
			lastErr = err
			continue
		}
		if int64(len(data)) != c.SizeBytes {
			lastErr = fmt.Errorf("size mismatch: got %d want %d", len(data), c.SizeBytes)
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != c.SHA256 {
			lastErr = fmt.Errorf("hash mismatch for chunk %d", c.Index)
			continue
		}
		if _, err := f.WriteAt(data, int64(c.Index)*jobprotocol.ChunkSize); err != nil {
			return "", err
		}
		return hd.PeerID, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no holders tried")
	}
	return "", lastErr
}

func downloadChunk(ctx context.Context, h host.Host, hd holder, modelID string, c chunkInfo) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := jobprotocol.ConnectPeer(ctx, h, hd.PeerID, hd.Addrs, hd.SourceIP); err != nil {
		return nil, err
	}
	pid, err := peer.Decode(hd.PeerID)
	if err != nil {
		return nil, err
	}
	s, err := h.NewStream(ctx, pid, jobprotocol.ChunkProtocol)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := json.NewEncoder(s).Encode(jobprotocol.ChunkRequest{ModelID: modelID, Index: c.Index}); err != nil {
		return nil, err
	}
	_ = s.CloseWrite()
	var res jobprotocol.ChunkResponse
	if err := json.NewDecoder(s).Decode(&res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("remote: %s", res.Error)
	}
	return res.Data, nil
}

func verifyFile(path string, sizeBytes int64, sha256hex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != sizeBytes {
		return fmt.Errorf("size mismatch: got %d want %d", fi.Size(), sizeBytes)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha256hex {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, sha256hex)
	}
	return nil
}
