package engine

import (
	"net/http"
	"strings"
	"time"
)

// RemoteEngine implements Engine by talking to an already-running llama-server.
// It never starts or stops a process — for sharing one engine between the
// API service and the node on memory-constrained boxes (e.g. Oracle 1GB),
// where loading the same model twice would OOM.
//
// The owning side (typically dain-node) still runs a Manager with cgroup
// limits; this side only proxies inference traffic to its BaseURL.
type RemoteEngine struct {
	baseURL string
	client  *http.Client
}

// NewRemote creates a RemoteEngine for the given base URL
// (e.g. "http://127.0.0.1:18082"). Trailing slashes are trimmed.
func NewRemote(baseURL string) *RemoteEngine {
	return &RemoteEngine{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

// BaseURL returns the remote engine's base URL.
func (r *RemoteEngine) BaseURL() string { return r.baseURL }

// Ready polls the remote /health endpoint, mirroring Manager.waitReady's
// check. False until the owning side has the engine up.
func (r *RemoteEngine) Ready() bool {
	resp, err := r.client.Get(r.baseURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Ensure RemoteEngine satisfies the Engine interface.
var _ Engine = (*RemoteEngine)(nil)
