// Package protocol defines the node-to-node wire protocol.
//
// Wire compatibility is load-bearing: protocol ID and ResourceInfo JSON field
// names must not change without a protocol version bump, or existing
// deployed nodes stop understanding each other.
package protocol

import "github.com/libp2p/go-libp2p/core/protocol"

// ResourceProtocol is the stream protocol for resource info exchange.
// v0.4.0 is fire-and-forget: no acknowledgement, and the receiver does
// not verify that the payload's self-declared PeerID matches the
// connection's authenticated RemotePeer. Deployed v0.10-alpha nodes
// speak only this version, so it is preserved unchanged.
const ResourceProtocol = protocol.ID("/distributed-ai/resource/0.4.0")

// ResourceProtocolV2 adds a receiver->sender acknowledgement and a
// hard identity rule: the receiver accepts the info ONLY when
// info.PeerID equals the stream's RemotePeer (the libp2p-authenticated
// identity), and says so in the ACK. Senders that need delivery proof
// use V2; senders talking to old nodes fall back to v0.4.0 and report
// the exchange as unacknowledged.
const ResourceProtocolV2 = protocol.ID("/distributed-ai/resource/0.5.0")

// Message size caps (security, 2026-10-25). A legitimate ResourceInfo is a
// few kilobytes of short strings; anything larger is abuse, not data.
// The relay's per-circuit limit is a transport backstop, not a substitute:
// the application layer must refuse oversized input before decoding it.
const (
	// MaxResourceInfoBytes caps one ResourceInfo message (V1 and V2).
	MaxResourceInfoBytes = 64 << 10
	// MaxResourceAckBytes caps one ResourceAck frame.
	MaxResourceAckBytes = 4 << 10
)

// ResourceAck is the V2 reply frame, written by the receiver after it
// has read (and judged) the ResourceInfo.
type ResourceAck struct {
	// OK: the info was accepted.
	OK bool `json:"ok"`
	// PeerID echoes the authenticated RemotePeer the receiver saw.
	PeerID string `json:"peer_id"`
	// Error explains a rejection (e.g. "peer_id_mismatch").
	Error string `json:"error,omitempty"`
}

// ResourceInfo is the node's advertised hardware and share config.
// JSON field names are frozen for wire compatibility.
type ResourceInfo struct {
	NodeName      string  `json:"node_name"`
	PeerID        string  `json:"peer_id"`
	OS            string  `json:"os"`
	Architecture  string  `json:"architecture"`
	CPUModel      string  `json:"cpu_model"`
	PhysicalCores int     `json:"physical_cores"`
	LogicalCores  int     `json:"logical_cores"`
	RAMTotalGB    float64 `json:"ram_total_gb"`

	GPUModel     string `json:"gpu_model"`
	GPUVendor    string `json:"gpu_vendor"`
	GPUCores     int    `json:"gpu_cores"`
	GPUDriver    string `json:"gpu_driver"`
	MetalSupport string `json:"metal_support"`

	StorageTotalGB float64 `json:"storage_total_gb"`
	StorageUsedGB  float64 `json:"storage_used_gb"`
	StorageFreeGB  float64 `json:"storage_free_gb"`

	CPUShare     int `json:"cpu_share"`
	GPUShare     int `json:"gpu_share"`
	RAMShare     int `json:"ram_share"`
	StorageShare int `json:"storage_share"`
}
