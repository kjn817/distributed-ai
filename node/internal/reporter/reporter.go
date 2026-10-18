// Package reporter reports the node to the central service over HTTP.
//
// The service aggregates these reports into /network/status. All values are
// the SHARED amounts (total × share percent), matching the dashboard's
// "Shared" semantics. Nothing here is mocked: every number comes from the
// node's real hardware detection and the user's real share config.
package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"distributed-ai/node/internal/protocol"
)

// nodePayload is the service's registry.Node JSON shape.
type nodePayload struct {
	ID             string   `json:"id"`
	OS             string   `json:"os"`
	CPUCores       int      `json:"cpu_cores"`
	CPUModel       string   `json:"cpu_model"`
	GPUModel       string   `json:"gpu_model"`
	VRAMBytes      int64    `json:"vram_bytes"`
	RAMBytes       int64    `json:"ram_bytes"`
	SSDSharedBytes int64    `json:"ssd_shared_bytes"`
	DirectOK       bool     `json:"direct_ok"`
	Addrs          []string `json:"addrs,omitempty"`
	Models         []string `json:"models,omitempty"`
	RpcAddr        string   `json:"rpc_addr,omitempty"`
}

const gb = 1024 * 1024 * 1024

func shared(total float64, percent int) int64 {
	if percent <= 0 || total <= 0 {
		return 0
	}
	return int64(math.Round(total * float64(percent) / 100))
}

// SharedCores returns the CPU cores this node actually shares (total ×
// share %). The engine thread limit enforces it — see main.go.
func SharedCores(logicalCores, cpuShare int) int {
	if n := int(shared(float64(logicalCores), cpuShare)); n > 0 {
		return n
	}
	return 1 // never starve the engine entirely
}

// Map converts ResourceInfo into the service's node payload, applying the
// user's share percentages. directOK is always false here: inbound
// reachability can only be verified by the service dialing back (M3
// scheduler), never self-reported. addrs are the node's libp2p listen
// addresses so the service knows what to dial.
func Map(info protocol.ResourceInfo, addrs []string, models []string, rpcAddr string) nodePayload {
	gpuModel := info.GPUModel
	if gpuModel == "Unknown" {
		gpuModel = "" // no phantom GPUs in the registry
	}
	return nodePayload{
		ID:             info.PeerID,
		OS:             info.OS,
		CPUCores:       int(shared(float64(info.LogicalCores), info.CPUShare)),
		CPUModel:       info.CPUModel,
		GPUModel:       gpuModel,
		VRAMBytes:      0, // ResourceInfo carries no VRAM bytes; unknown, not guessed
		RAMBytes:       shared(info.RAMTotalGB, info.RAMShare) * gb,
		SSDSharedBytes: shared(info.StorageFreeGB, info.StorageShare) * gb,
		DirectOK:       false,
		Addrs:          addrs,
		Models:         models,
		RpcAddr:        rpcAddr,
	}
}

// statusError carries the HTTP status of a failed reporter call so callers
// can distinguish "unknown node" (404) from other failures.
type statusError struct {
	op   string
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("reporter: %s: http %d", e.op, e.code)
}

// IsNotFound reports whether err is an HTTP 404 from the service — e.g.
// the service restarted and no longer knows this node.
func IsNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == http.StatusNotFound
}

// rpcAddrOf extracts the RPC worker address from cfg ("" when not a worker).
func rpcAddrOf(cfg Config) string {
	if cfg.RpcAddr == nil {
		return ""
	}
	return cfg.RpcAddr()
}

// Register POSTs the node to /v1/nodes/register.
func Register(ctx context.Context, apiURL string, info protocol.ResourceInfo, addrs []string, models []string, rpcAddr string) error {
	body, err := json.Marshal(Map(info, addrs, models, rpcAddr))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL+"/v1/nodes/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &statusError{op: "register", code: resp.StatusCode}
	}
	return nil
}

// heartbeatResponse is what the service returns for a heartbeat:
// liveness ack plus any replication work assigned to this node.
type heartbeatResponse struct {
	OK        bool     `json:"ok"`
	Replicate []string `json:"replicate"`
}

// ReceivedFrom is a receiver-confirmed chunk transfer report: this node
// received Bytes from PeerID. The service credits the SENDER on this basis.
type ReceivedFrom struct {
	PeerID string `json:"peer_id"`
	Bytes  int64  `json:"bytes"`
}

// LimitsReport is the OS resource-cap enforcement status.
type LimitsReport struct {
	Enforced  bool   `json:"enforced"`
	Mechanism string `json:"mechanism,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Heartbeat POSTs liveness and returns model IDs the service wants this
// node to replicate (fetch over P2P).
func Heartbeat(ctx context.Context, cfg Config) ([]string, error) {
	var lim *LimitsReport
	if cfg.Limits != nil {
		if r := cfg.Limits(); r.Enforced || r.Reason != "" {
			lim = &r
		}
	}
	rpcAddr := ""
	if cfg.RpcAddr != nil {
		rpcAddr = cfg.RpcAddr()
	}
	body, _ := json.Marshal(map[string]any{
		"node_id":            cfg.Info.PeerID,
		"chunk_bytes_served": cfg.chunkBytes(),
		"chunk_received":     cfg.chunkReceived(),
		"limits":             lim,
		"rpc_addr":           rpcAddr,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.APIURL+"/v1/nodes/heartbeat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{op: "heartbeat", code: resp.StatusCode}
	}
	var hr heartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		return nil, fmt.Errorf("reporter: heartbeat: decode: %w", err)
	}
	return hr.Replicate, nil
}

// Config wires the reporting loop.
type Config struct {
	APIURL string
	Info   protocol.ResourceInfo
	Addrs  []string
	Models func() []string // fresh list on every (re-)register
	// ChunkBytes reports cumulative chunk payload bytes served (for
	// transfer metering); nil = not serving chunks.
	ChunkBytes func() int64
	// ChunkReceived drains receiver-confirmed bytes per source peer.
	// The service pays transfer credit on this, not on self-reports.
	ChunkReceived func() []ReceivedFrom
	// Limits reports OS cap enforcement (nil = unknown/not started).
	Limits func() LimitsReport
	// RpcAddr returns "host:port" of the rpc-server ("" = not a worker).
	RpcAddr  func() string
	Interval time.Duration
	OnErr    func(error)
	// OnAssign receives model IDs the service wants replicated; the node
	// fetches them in the background.
	OnAssign func(modelIDs []string)
}

// chunkBytes is nil-safe.
func (c Config) chunkBytes() int64 {
	if c.ChunkBytes == nil {
		return 0
	}
	return c.ChunkBytes()
}

// chunkReceived is nil-safe.
func (c Config) chunkReceived() []ReceivedFrom {
	if c.ChunkReceived == nil {
		return nil
	}
	return c.ChunkReceived()
}

// Run registers once, then heartbeats every interval until ctx is done.
// A failed heartbeat is reported via OnErr; the loop keeps trying (a
// transient network blip must not kill the node). If the service answers
// 404 — it restarted and forgot us — the node re-registers with the
// current models list instead of heartbeating into the void forever.
func Run(ctx context.Context, cfg Config) {
	if err := Register(ctx, cfg.APIURL, cfg.Info, cfg.Addrs, cfg.Models(), rpcAddrOf(cfg)); err != nil {
		cfg.OnErr(fmt.Errorf("initial register: %w", err))
		return
	}
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			assign, err := Heartbeat(ctx, cfg)
			if err != nil {
				if IsNotFound(err) {
					if rerr := Register(ctx, cfg.APIURL, cfg.Info, cfg.Addrs, cfg.Models(), rpcAddrOf(cfg)); rerr != nil {
						cfg.OnErr(fmt.Errorf("re-register: %w", rerr))
					} else {
						cfg.OnErr(errors.New("re-registered with service after heartbeat 404"))
					}
				} else {
					cfg.OnErr(err)
				}
				continue
			}
			if len(assign) > 0 && cfg.OnAssign != nil {
				cfg.OnAssign(assign)
			}
		}
	}
}
