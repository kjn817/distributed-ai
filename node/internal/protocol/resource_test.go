package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/libp2p/go-libp2p/core/protocol"
)

func TestProtocolIDFrozen(t *testing.T) {
	// Wire compatibility: existing deployed nodes speak this exact ID.
	if ResourceProtocol != protocol.ID("/distributed-ai/resource/0.4.0") {
		t.Fatalf("protocol ID changed: %s", ResourceProtocol)
	}
}

func TestResourceInfoJSONFieldNames(t *testing.T) {
	info := ResourceInfo{
		NodeName: "n1", PeerID: "p1", OS: "linux", Architecture: "amd64",
		CPUModel: "c", PhysicalCores: 4, LogicalCores: 8, RAMTotalGB: 16,
		GPUModel: "g", GPUVendor: "NVIDIA", GPUCores: 1, GPUDriver: "d",
		MetalSupport: "m", StorageTotalGB: 100, StorageUsedGB: 10,
		StorageFreeGB: 90, CPUShare: 10, GPUShare: 10, RAMShare: 10, StorageShare: 10,
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"node_name"`, `"peer_id"`, `"os"`, `"architecture"`,
		`"cpu_model"`, `"physical_cores"`, `"logical_cores"`, `"ram_total_gb"`,
		`"gpu_model"`, `"gpu_vendor"`, `"gpu_cores"`, `"gpu_driver"`, `"metal_support"`,
		`"storage_total_gb"`, `"storage_used_gb"`, `"storage_free_gb"`,
		`"cpu_share"`, `"gpu_share"`, `"ram_share"`, `"storage_share"`,
	} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("missing JSON field %s", key)
		}
	}
	// Round-trip preserves values.
	var back ResourceInfo
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back != info {
		t.Fatal("round-trip mismatch")
	}
}
