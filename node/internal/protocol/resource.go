// Package protocol defines the node-to-node wire protocol.
//
// Wire compatibility is load-bearing: protocol ID and ResourceInfo JSON field
// names must not change without a protocol version bump, or existing
// deployed nodes stop understanding each other.
package protocol

import "github.com/libp2p/go-libp2p/core/protocol"

// ResourceProtocol is the stream protocol for resource info exchange.
const ResourceProtocol = protocol.ID("/distributed-ai/resource/0.4.0")

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
