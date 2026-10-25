// Chunk protocol types: the wire format for fetching model chunks from peers.
//
// Chunks are 64MB storage/transfer units (see docs/manifest-spec.md) — they
// are NOT compute shards. A node holding a full model file serves any chunk
// as a byte range; the requester verifies each chunk's SHA-256 against the
// manifest before assembly.
//
// Protocol ID and JSON field names are frozen for wire compatibility.
// v1 framing is JSON (chunk bytes ride as base64); a binary framing upgrade
// is future work — correctness first.
package protocol

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
)

// ChunkSize is the fixed storage/transfer unit: 64 MiB.
const ChunkSize = 64 << 20

// ConnectPeer establishes a libp2p connection to the node at peerID,
// trying candidates in order:
//
//  1. The node's reported libp2p addresses (skipping unspecified 0.0.0.0).
//  2. The HTTP source IP the node registered from, combined with the p2p
//     ports parsed from its reported addrs.
//
// Returns nil once a connection is established. Shared by the service
// (dial-back verification, job dispatch) and nodes (chunk fetching).
func ConnectPeer(ctx context.Context, h host.Host, peerID string, addrs []string, sourceIP string) error {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return err
	}
	// Bound the connect phase: a hanging dial must not stall callers.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	try := func(maddr ma.Multiaddr) error {
		full := maddr.Encapsulate(ma.StringCast("/p2p/" + pid.String()))
		ai, err := peer.AddrInfoFromP2pAddr(full)
		if err != nil {
			return err
		}
		return h.Connect(ctx, *ai)
	}

	var tcpPorts []string
	var lastErr error
	for _, a := range addrs {
		maddr, err := ma.NewMultiaddr(a)
		if err != nil {
			continue
		}
		// Relayed (circuit) addresses are complete as they are: they
		// already end in the destination /p2p/<peerID>, and the TCP
		// port in them belongs to the RELAY — collecting it for the
		// source-IP fallback would dial the wrong machine entirely.
		// Validate the embedded peer ID and dial the address as-is.
		if strings.Contains(a, "/p2p-circuit") {
			ai, err := peer.AddrInfoFromP2pAddr(maddr)
			if err != nil {
				lastErr = err
				continue
			}
			if ai.ID != pid {
				lastErr = fmt.Errorf("circuit address peer %s does not match target %s", ai.ID, pid)
				continue
			}
			if err := h.Connect(ctx, *ai); err == nil {
				return nil
			} else {
				lastErr = err
			}
			continue
		}
		tcpPorts = append(tcpPorts, tcpPortsOf(maddr)...)
		if isUnspecified(maddr) {
			continue
		}
		if err := try(maddr); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}

	for _, port := range tcpPorts {
		var maddr ma.Multiaddr
		if strings.Contains(sourceIP, ":") {
			maddr, err = ma.NewMultiaddr("/ip6/" + sourceIP + "/tcp/" + port)
		} else {
			maddr, err = ma.NewMultiaddr("/ip4/" + sourceIP + "/tcp/" + port)
		}
		if err != nil {
			continue
		}
		if err := try(maddr); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no dialable addresses")
	}
	return lastErr
}

// tcpPortsOf extracts the TCP port numbers from a multiaddr.
func tcpPortsOf(maddr ma.Multiaddr) []string {
	var ports []string
	parts := strings.Split(maddr.String(), "/")
	for i, p := range parts {
		if p == "tcp" && i+1 < len(parts) {
			ports = append(ports, parts[i+1])
		}
	}
	return ports
}

func isUnspecified(maddr ma.Multiaddr) bool {
	s := maddr.String()
	return len(s) >= 12 && s[:12] == "/ip4/0.0.0.0"
}

// ChunkProtocol is the stream protocol for chunk transfer.
const ChunkProtocol = protocol.ID("/distributed-ai/chunk/1")

// ChunkRequest asks a peer for one chunk of a model file.
type ChunkRequest struct {
	ModelID string `json:"model_id"`
	Index   int    `json:"index"`
}

// ChunkResponse carries one chunk's bytes, or an error.
type ChunkResponse struct {
	ModelID string `json:"model_id"`
	Index   int    `json:"index"`
	Data    []byte `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}
