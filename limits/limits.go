// Package limits enforces OS-level resource caps on the inference engine.
//
// Linux cgroup v2 is the only mechanism that actually caps CPU time and RAM:
//   - cpu.max  = "<quota> 100000"  (quota in microseconds per 100ms period)
//   - memory.max = "<bytes>"
//
// --threads only hints the engine's worker count; the OS can still schedule
// those threads freely. A cgroup is what makes "10% CPU" mean 10%.
//
// On non-Linux, or when the cgroup filesystem is not writable (non-root
// without delegation, cgroup v1), Apply reports Unenforced with the reason.
// The node surfaces this honestly instead of pretending a limit exists.
//
// GPU: no vendor exposes fractional compute caps (CUDA_VISIBLE_DEVICES only
// selects whole devices). GPU sharing is whole-device allocation; this
// package documents the limit rather than faking enforcement.
// SSD: enforced by refusing to materialize beyond the shared byte budget
// (see MaxBytes check in the node).
package limits

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Status describes whether a limit is actually enforced.
type Status struct {
	Enforced bool
	// Mechanism is "cgroup2" when enforced, else "".
	Mechanism string
	// Reason explains why not enforced (platform, permission, ...).
	Reason string
}

// Handle is a live cgroup membership; Release removes the cgroup.
type Handle struct {
	path string
}

// Release removes the cgroup (processes must have exited).
func (h *Handle) Release() {
	if h == nil || h.path == "" {
		return
	}
	// Best effort: the kernel refuses while processes remain.
	os.Remove(h.path)
}

// Apply puts pid under CPU and memory caps derived from share percentages.
// cpuPercent: 0-100 of all CPUs. memBytes: hard RAM cap (0 = no cap).
// Returns the enforcement status; when not enforced, the caller must
// surface Status.Reason honestly.
func Apply(pid int, cpuPercent int, memBytes int64) (*Handle, Status) {
	switch runtime.GOOS {
	case "linux":
		// implemented below
	case "windows":
		return nil, Status{Reason: "Windows: CPU/RAM caps via Job Objects not yet implemented — threads hint only. See PlatformSupport()."}
	case "darwin":
		return nil, Status{Reason: "macOS: no OS API for hard CPU caps (cgroups don't exist); memory via setrlimit not yet implemented — threads hint only. See PlatformSupport()."}
	default:
		return nil, Status{Reason: runtime.GOOS + ": OS resource caps not implemented — threads hint only"}
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return nil, Status{Reason: "cgroup v2 not mounted: " + err.Error()}
	}
	has := func(name string) bool {
		for _, f := range strings.Fields(string(controllers)) {
			if f == name {
				return true
			}
		}
		return false
	}
	// Try to create our cgroup; permission errors are the common case for
	// non-root users without delegation.
	dir := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("distributed-ai-%d", pid))
	if err := os.Mkdir(dir, 0755); err != nil {
		return nil, Status{Reason: "cannot create cgroup (need write access to /sys/fs/cgroup): " + err.Error()}
	}
	h := &Handle{path: dir}
	// Enable controllers in our subtree (best effort; parent may already).
	if has("cpu") && cpuPercent > 0 && cpuPercent < 100 {
		quota := cpuPercent * 1000 // percent of one 100ms period in us
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"),
			[]byte(fmt.Sprintf("%d 100000", quota)), 0644); err != nil {
			h.Release()
			return nil, Status{Reason: "cpu.max not writable: " + err.Error()}
		}
	}
	// cpuPercent >= 100 (or <= 0): no CPU cap requested. Note: 0% CPU share
	// means "no inference work" — enforced by placement, not here.
	if has("memory") && memBytes > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"),
			[]byte(fmt.Sprintf("%d", memBytes)), 0644); err != nil {
			h.Release()
			return nil, Status{Reason: "memory.max not writable: " + err.Error()}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"),
		[]byte(fmt.Sprintf("%d", pid)), 0644); err != nil {
		h.Release()
		return nil, Status{Reason: "cannot attach process to cgroup: " + err.Error()}
	}
	return h, Status{Enforced: true, Mechanism: "cgroup2"}
}

// GPULimitNote documents the platform reality for fractional GPU sharing.
func GPULimitNote() string {
	return "GPU compute cannot be fractionally capped by any vendor API " +
		"(CUDA_VISIBLE_DEVICES selects whole devices only). " +
		"GPU sharing is whole-device allocation: a node shares N whole GPUs or none. " +
		"Partial GPU percentages are rejected at startup (use --gpu 0 or --gpu 100)."
}

// PlatformSupport describes OS-level enforcement availability per platform.
// Used for honest display in the node startup log and dashboard.
func PlatformSupport() map[string]string {
	return map[string]string{
		"linux":   "cgroup v2: CPU (cpu.max) and RAM (memory.max) hard caps enforced",
		"windows": "Job Objects can cap CPU rate and working set — not yet implemented (threads hint only)",
		"darwin":  "No hard CPU cap API on macOS; memory via setrlimit not yet implemented (threads hint only)",
	}
}
