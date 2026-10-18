// Package engine manages the local llama.cpp inference engine process
// (llama-server binary) and exposes it to the API layer.
//
// v1: the central service spawns one local engine (the Oracle node's own
// inference). Remote worker engines arrive with M3's job protocol.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"distributed-ai/limits"
)

// Config describes how to launch the engine.
type Config struct {
	BinaryPath string   // path to llama-server binary
	ModelPath  string   // path to GGUF model file
	Port       int      // localhost port for the engine
	CtxSize    int      // context size (0 = engine default)
	Threads    int      // CPU threads (0 = engine default)
	CPUPercent int      // OS CPU cap 0-100 (0 = no cap); Linux cgroup v2
	MemBytes   int64    // OS RAM cap in bytes (0 = no cap); Linux cgroup v2
	ExtraArgs  []string // passed through verbatim
}

// Engine is the interface the API layer needs.
type Engine interface {
	Ready() bool
	BaseURL() string
}

// Manager owns the engine subprocess.
type Manager struct {
	cfg          Config
	cmd          *exec.Cmd
	ready        atomic.Bool
	limitsHandle *limits.Handle
	limitsStatus limits.Status
}

// New creates a Manager. It does not start the process.
func New(cfg Config) *Manager { return &Manager{cfg: cfg} }

func (m *Manager) Ready() bool { return m.ready.Load() }

func (m *Manager) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", m.cfg.Port)
}

// Start launches llama-server and waits until /health reports ok.
func (m *Manager) Start(ctx context.Context) error {
	if m.cfg.BinaryPath == "" {
		return fmt.Errorf("engine: binary path not configured")
	}
	if m.cfg.ModelPath == "" {
		return fmt.Errorf("engine: model path not configured")
	}
	if !portFree(m.cfg.Port) {
		return fmt.Errorf("engine: port %d already in use", m.cfg.Port)
	}

	args := []string{
		"--model", m.cfg.ModelPath,
		"--port", fmt.Sprint(m.cfg.Port),
		"--host", "127.0.0.1",
	}
	if m.cfg.CtxSize > 0 {
		args = append(args, "--ctx-size", fmt.Sprint(m.cfg.CtxSize))
	}
	if m.cfg.Threads > 0 {
		args = append(args, "--threads", fmt.Sprint(m.cfg.Threads))
	}
	args = append(args, m.cfg.ExtraArgs...)

	m.cmd = exec.CommandContext(ctx, m.cfg.BinaryPath, args...)
	m.cmd.Stdout = nil // engine logs go to its own stderr; keep ours clean
	// llama-server built from source links against libggml*.so living next
	// to the binary. Make sure the loader finds them.
	m.cmd.Env = withLibPath(os.Environ(), filepath.Dir(m.cfg.BinaryPath))
	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("engine: failed to start: %w", err)
	}
	// OS-level resource caps (Linux cgroup v2). Best effort: when the
	// cgroup cannot be created, the limit status is recorded honestly and
	// the engine still runs (threads hint only).
	if m.cfg.CPUPercent > 0 && m.cfg.CPUPercent < 100 || m.cfg.MemBytes > 0 {
		h, st := limits.Apply(m.cmd.Process.Pid, m.cfg.CPUPercent, m.cfg.MemBytes)
		m.limitsStatus = st
		if h != nil {
			m.limitsHandle = h
		}
		fmt.Printf("engine limits: cpu=%d%% mem=%d enforced=%v mech=%s reason=%s\n",
			m.cfg.CPUPercent, m.cfg.MemBytes, st.Enforced, st.Mechanism, st.Reason)
	}

	if err := m.waitReady(ctx, 120*time.Second); err != nil {
		_ = m.Stop()
		return err
	}
	m.ready.Store(true)
	return nil
}

// LimitsStatus reports whether OS resource caps are enforced.
func (m *Manager) LimitsStatus() limits.Status { return m.limitsStatus }

// waitReady polls /health until the engine reports ok or timeout.
func (m *Manager) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := m.BaseURL() + "/health"
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if healthOK(url) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("engine: not ready after %s", timeout)
}

func healthOK(url string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.Status == "ok"
}

// Stop terminates the engine process.
func (m *Manager) Stop() error {
	m.ready.Store(false)
	if m.cmd == nil || m.cmd.Process == nil {
		return nil
	}
	if err := m.cmd.Process.Kill(); err != nil {
		return err
	}
	_, _ = m.cmd.Process.Wait()
	if m.limitsHandle != nil {
		m.limitsHandle.Release()
		m.limitsHandle = nil
	}
	return nil
}

func portFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// withLibPath prepends dir to LD_LIBRARY_PATH (Linux) so a source-built
// llama-server finds its libggml*.so siblings.
func withLibPath(env []string, dir string) []string {
	const key = "LD_LIBRARY_PATH"
	for i, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			env[i] = key + "=" + dir + ":" + strings.TrimPrefix(kv, key+"=")
			return env
		}
	}
	return append(env, key+"="+dir)
}
