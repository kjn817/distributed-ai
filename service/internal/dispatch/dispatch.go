// Package dispatch sends inference jobs to worker nodes over
// /distributed-ai/job/1 and reads back the result.
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	jobprotocol "distributed-ai/protocol"
	"distributed-ai/service/internal/p2pnet"
	"distributed-ai/service/internal/registry"
)

// Dispatch sends req to the node and returns its JobResult. The node must
// already be dial-back verified; ConnectNode reuses (or re-establishes) the
// connection.
func Dispatch(ctx context.Context, h host.Host, node *registry.Node, req *jobprotocol.JobRequest) (*jobprotocol.JobResult, error) {
	if err := p2pnet.ConnectNode(ctx, h, node.ID, node.Addrs, node.SourceIP); err != nil {
		return nil, fmt.Errorf("dispatch: connect: %w", err)
	}
	pid, err := peer.Decode(node.ID)
	if err != nil {
		return nil, fmt.Errorf("dispatch: bad peer id: %w", err)
	}
	s, err := h.NewStream(ctx, pid, jobprotocol.JobProtocol)
	if err != nil {
		return nil, fmt.Errorf("dispatch: new stream: %w", err)
	}
	defer s.Close()

	if err := json.NewEncoder(s).Encode(req); err != nil {
		return nil, fmt.Errorf("dispatch: write request: %w", err)
	}
	_ = s.CloseWrite()

	var res jobprotocol.JobResult
	if err := json.NewDecoder(s).Decode(&res); err != nil {
		return nil, fmt.Errorf("dispatch: read result: %w", err)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("dispatch: remote error: %s", res.Error)
	}
	return &res, nil
}
