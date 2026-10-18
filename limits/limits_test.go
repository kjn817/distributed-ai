package limits

import (
	"runtime"
	"testing"
)

func TestApplyReportsHonestly(t *testing.T) {
	h, st := Apply(12345, 10, 1<<30)
	if h != nil {
		h.Release()
	}
	// In this test env cgroup v2 is not writable; the contract is that
	// Apply never pretends success.
	if runtime.GOOS != "linux" {
		if st.Enforced || st.Reason == "" {
			t.Fatalf("non-Linux must report unenforced with reason, got %+v", st)
		}
		return
	}
	// On Linux without cgroup access, must also report honestly.
	if st.Enforced && st.Mechanism == "" {
		t.Fatalf("enforced without mechanism: %+v", st)
	}
	t.Logf("status: %+v", st)
}

func TestGPULimitNote(t *testing.T) {
	if n := GPULimitNote(); n == "" {
		t.Fatal("empty GPU limit note")
	}
}
