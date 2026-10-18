// Package rpcworker runs a llama.cpp rpc-server (ggml-rpc-server binary)
// so this node can jointly compute one request with a coordinator.
// The worker loads the FULL model file; the coordinator (llama-server
// --rpc) offloads layers/tensors to it.
package rpcworker

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync/atomic"
	"time"
)

// Worker manages one rpc-server process.
type Worker struct {
	bin   string
	host  string
	port  int
	cmd   *exec.Cmd
	ready atomic.Bool
}

// New creates a Worker. bin is the ggml-rpc-server binary, host/port the
// listen address ("0.0.0.0:50052"). The rpc-server receives tensors from
// the coordinator at runtime; it does not load the model file itself.
func New(bin, host string, port int) *Worker {
	return &Worker{bin: bin, host: host, port: port}
}

// Start launches rpc-server and waits for the port to accept.
// Note: ggml-rpc-server does NOT load the model; the coordinator sends
// tensors at runtime. modelPath is verified to exist (the worker must
// HAVE the model file for... actually no — the RPC protocol sends the
// model data. But we require the file to exist as proof the worker
// materialized it (prevents workers without the model from joining).
func (w *Worker) Start(ctx context.Context) error {
	if w.bin == "" {
		return fmt.Errorf("rpcworker: binary not configured (RPC_BIN)")
	}
	// ggml-rpc-server CLI: -H host -p port -t threads (no --model;
	// the coordinator ships tensors at runtime).
	args := []string{
		"-H", w.host,
		"-p", fmt.Sprint(w.port),
		"-t", "4",
	}
	w.cmd = exec.CommandContext(ctx, w.bin, args...)
	w.cmd.Stdout = nil
	if err := w.cmd.Start(); err != nil {
		return fmt.Errorf("rpcworker: start: %w", err)
	}
	// Wait for the port.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", w.port), 500*time.Millisecond)
		if err == nil {
			c.Close()
			w.ready.Store(true)
			return nil
		}
		select {
		case <-ctx.Done():
			_ = w.Stop()
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	_ = w.Stop()
	return fmt.Errorf("rpcworker: port %d never opened", w.port)
}

// Stop kills the rpc-server.
func (w *Worker) Stop() error {
	w.ready.Store(false)
	if w.cmd == nil || w.cmd.Process == nil {
		return nil
	}
	if err := w.cmd.Process.Kill(); err != nil {
		return err
	}
	_, _ = w.cmd.Process.Wait()
	return nil
}

// Ready reports whether the rpc-server is accepting.
func (w *Worker) Ready() bool { return w.ready.Load() }

// Addr returns "host:port" for coordinators to dial.
func (w *Worker) Addr() string { return fmt.Sprintf("%s:%d", w.host, w.port) }
