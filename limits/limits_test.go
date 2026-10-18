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

func TestDetectExistingNeverClaimsUnlimited(t *testing.T) {
	st := DetectExisting()
	// The test env has no systemd limits; neither resource may be
	// reported enforced.
	if st.CPUEnforced {
		t.Fatalf("CPU reported enforced without a limit: %+v", st)
	}
	if st.RAMEnforced {
		t.Fatalf("RAM reported enforced without a limit: %+v", st)
	}
	if st.Enforced {
		t.Fatalf("overall enforced with no per-resource limits: %+v", st)
	}
}

func TestApplyFallbackHonestPerResource(t *testing.T) {
	// In this env Apply cannot create its own cgroup; it must fall back
	// to DetectExisting and never claim success without real limits.
	_, st := Apply(12345, 70, 700*1024*1024)
	if st.Enforced {
		t.Fatalf("must not report enforced without real limits: %+v", st)
	}
	if st.CPUEnforced || st.RAMEnforced {
		t.Fatalf("per-resource must be false without limits: %+v", st)
	}
	if st.Reason == "" {
		t.Fatal("must explain why not enforced")
	}
}

func TestStatusPerResourceFields(t *testing.T) {
	// Zero-value Status must not claim anything.
	var st Status
	if st.Enforced || st.CPUEnforced || st.RAMEnforced {
		t.Fatalf("zero Status must be all-false: %+v", st)
	}
}
