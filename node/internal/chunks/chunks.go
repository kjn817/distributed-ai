// Package chunks serves model chunks to peers over /distributed-ai/chunk/1.
//
// v1: a node holding a full model file serves any chunk as a byte range
// (offset = index × ChunkSize). Chunked-only storage (serving without the
// full file) is future work — v1 is a FULL_MATERIALIZE world.
package chunks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/libp2p/go-libp2p/core/network"

	jobprotocol "distributed-ai/protocol"
)

// Server serves chunks of locally held model files.
type Server struct {
	// lookup returns the local file path for a model ID.
	lookup func(modelID string) (string, bool)
	// bytesServed counts chunk payload bytes served (for credit metering).
	bytesServed atomic.Int64
}

// New creates a chunk server with the given model lookup.
func New(lookup func(modelID string) (string, bool)) *Server {
	return &Server{lookup: lookup}
}

// BytesServed returns total chunk bytes served so far.
func (s *Server) BytesServed() int64 { return s.bytesServed.Load() }

// HandleStream serves one ChunkRequest per stream.
func (s *Server) HandleStream(stream network.Stream) {
	defer stream.Close()
	var req jobprotocol.ChunkRequest
	if err := json.NewDecoder(stream).Decode(&req); err != nil {
		return
	}
	resp := jobprotocol.ChunkResponse{ModelID: req.ModelID, Index: req.Index}
	path, ok := s.lookup(req.ModelID)
	if !ok {
		resp.Error = fmt.Sprintf("model %q not held by this node", req.ModelID)
		_ = json.NewEncoder(stream).Encode(resp)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		resp.Error = fmt.Sprintf("open model file: %v", err)
		_ = json.NewEncoder(stream).Encode(resp)
		return
	}
	defer f.Close()

	offset := int64(req.Index) * jobprotocol.ChunkSize
	buf := make([]byte, jobprotocol.ChunkSize)
	n, err := readAtFull(f, buf, offset)
	if err != nil {
		resp.Error = fmt.Sprintf("read chunk %d: %v", req.Index, err)
		_ = json.NewEncoder(stream).Encode(resp)
		return
	}
	if n == 0 {
		resp.Error = fmt.Sprintf("chunk %d beyond end of file", req.Index)
		_ = json.NewEncoder(stream).Encode(resp)
		return
	}
	resp.Data = buf[:n]
	s.bytesServed.Add(int64(n))
	_ = json.NewEncoder(stream).Encode(resp)
}

func readAtFull(f *os.File, buf []byte, offset int64) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.ReadAt(buf[total:], offset+int64(total))
		total += n
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
