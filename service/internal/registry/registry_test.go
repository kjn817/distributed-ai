package registry

import "testing"

func TestAddTransferBytes(t *testing.T) {
	reg := New()
	reg.Register(&Node{ID: "n1"})
	reg.Register(&Node{ID: "n2"})
	const GB = 1024 * 1024 * 1024
	// Receiver-confirmed: n2 reports receiving 1.5GB from n1 → 1GB credited.
	if got := reg.AddTransferBytes("n1", GB+GB/2, GB); got != GB {
		t.Fatalf("expected 1GB credited, got %d", got)
	}
	// Dust accumulates: another 0.5GB completes the second GB.
	if got := reg.AddTransferBytes("n1", GB/2, GB); got != GB {
		t.Fatalf("expected 1GB credited, got %d", got)
	}
	// Unknown sender → 0 (no minting credit for ghosts).
	if got := reg.AddTransferBytes("nope", 10*GB, GB); got != 0 {
		t.Fatalf("expected 0 for unknown sender, got %d", got)
	}
	// Non-positive → 0.
	if got := reg.AddTransferBytes("n1", 0, GB); got != 0 {
		t.Fatalf("expected 0 for zero bytes, got %d", got)
	}
}
