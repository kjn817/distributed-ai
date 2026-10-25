package p2p

// Security tests for the relay + ResourceInfo path (2026-10-25).
// These are attack tests, not happy-path tests: each one sends something
// a malicious or malfunctioning peer would send and asserts the system
// refuses it, logs it, and keeps serving legitimate peers.
// They run against real libp2p hosts on loopback — no mocks.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRelayDefaultDeniesEmptyAllowlist: an empty allowlist must never
// silently become an open relay. Starting a relay with no members and
// no explicit opt-in is an error (default deny).
func TestRelayDefaultDeniesEmptyAllowlist(t *testing.T) {
	relayKey, _ := keyAndID(t)
	_, err := NewRelayHost(relayKey, RelayConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err == nil {
		t.Fatal("NewRelayHost with an empty allowlist succeeded; want default-deny error")
	}
	if !strings.Contains(err.Error(), "allowlist is empty") {
		t.Fatalf("error = %q, want it to mention the empty allowlist", err.Error())
	}
	t.Logf("empty allowlist refused as expected: %v", err)
}

// TestRelayAllowAllRequiresExplicitOptIn: allow-all still exists for
// isolated local experiments, but only when the caller says so out loud.
func TestRelayAllowAllRequiresExplicitOptIn(t *testing.T) {
	relayKey, _ := keyAndID(t)
	relay, err := NewRelayHost(relayKey, RelayConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		AllowAll:    true,
	})
	if err != nil {
		t.Fatalf("AllowAll relay: %v", err)
	}
	defer relay.Close()
	t.Log("explicit AllowAll relay starts (local-test-only mode)")
}

// TestDefaultRelayLimits pins the server-side caps so a future edit
// cannot silently raise them: per-peer reservations, per-IP
// reservations, circuit count, and the per-circuit data cap that
// bounds total relayed traffic (32 circuits x 16 MiB = 512 MiB).
func TestDefaultRelayLimits(t *testing.T) {
	r := defaultRelayResources()
	if r.ReservationTTL != time.Hour {
		t.Fatalf("ReservationTTL = %v, want 1h", r.ReservationTTL)
	}
	if r.Limit == nil || r.Limit.Duration != time.Hour {
		t.Fatalf("per-circuit Duration = %v, want 1h", r.Limit)
	}
	if r.Limit.Data != 16<<20 {
		t.Fatalf("per-circuit Data = %d, want 16 MiB", r.Limit.Data)
	}
	if r.MaxReservations != 512 {
		t.Fatalf("MaxReservations = %d, want 512", r.MaxReservations)
	}
	if r.MaxCircuits != 32 {
		t.Fatalf("MaxCircuits = %d, want 32", r.MaxCircuits)
	}
	if r.MaxReservationsPerPeer != 4 {
		t.Fatalf("MaxReservationsPerPeer = %d, want 4", r.MaxReservationsPerPeer)
	}
	if r.MaxReservationsPerIP != 64 {
		t.Fatalf("MaxReservationsPerIP = %d, want 64", r.MaxReservationsPerIP)
	}
	total := int64(r.MaxCircuits) * r.Limit.Data
	if total != 512<<20 {
		t.Fatalf("structural total cap = %d bytes, want 512 MiB", total)
	}
	t.Logf("relay caps: %d circuits x 16 MiB = 512 MiB total; %d reservations, %d per peer, %d per IP",
		r.MaxCircuits, r.MaxReservations, r.MaxReservationsPerPeer, r.MaxReservationsPerIP)
}

// TestResourceV2RejectsOversizedMessage: a ResourceInfo larger than
// MaxResourceInfoBytes (64 KiB) is rejected with message_too_large and
// never delivered — the receiver must not decode unbounded input.
func TestResourceV2RejectsOversizedMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got := make(chan receipt, 4)
	svcKey, _ := keyAndID(t)
	svc := newSvcHost(t, svcKey, got)
	defer svc.Close()
	nodeKey, nodeID := keyAndID(t)
	node, err := NewWithAddrs(nodeKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	big := sampleInfo(nodeID)
	big.CPUModel = strings.Repeat("x", 128<<10) // ~128 KiB > 64 KiB cap
	if _, err := ConnectAndSendV2(ctx, node, tcpAddrOf(t, svc), big); err == nil {
		t.Fatal("oversized ResourceInfo was accepted; want message_too_large rejection")
	} else if !strings.Contains(err.Error(), "message_too_large") {
		t.Fatalf("error = %q, want message_too_large", err.Error())
	}
	select {
	case r := <-got:
		t.Fatalf("oversized info was delivered to the receiver: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
	t.Logf("oversized (%d bytes) ResourceInfo rejected, never delivered", len(big.CPUModel))
}

// TestResourceV2RateLimitsAbnormalRequests: one peer hammering the
// receiver with ResourceInfo is cut off after the per-peer limit
// (10/minute); a different peer is unaffected (the limit is per-peer,
// not global).
func TestResourceV2RateLimitsAbnormalRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got := make(chan receipt, 32)
	svcKey, _ := keyAndID(t)
	svc := newSvcHost(t, svcKey, got)
	defer svc.Close()
	svcAddr := tcpAddrOf(t, svc)

	nodeKey, nodeID := keyAndID(t)
	node, err := NewWithAddrs(nodeKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	accepted, rejected := 0, 0
	for i := 0; i < 12; i++ {
		_, err := ConnectAndSendV2(ctx, node, svcAddr, sampleInfo(nodeID))
		if err == nil {
			accepted++
		} else if strings.Contains(err.Error(), "rate_limited") {
			rejected++
		} else {
			t.Fatalf("send %d: unexpected error %v", i+1, err)
		}
	}
	if accepted != 10 {
		t.Fatalf("accepted = %d, want 10 (the per-peer per-minute limit)", accepted)
	}
	if rejected != 2 {
		t.Fatalf("rejected = %d, want 2 rate_limited", rejected)
	}
	// A different peer is not punished for the first peer's abuse.
	otherKey, otherID := keyAndID(t)
	other, err := NewWithAddrs(otherKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := ConnectAndSendV2(ctx, other, svcAddr, sampleInfo(otherID)); err != nil {
		t.Fatalf("second peer send: %v (rate limit must be per-peer)", err)
	}
	// Exactly 11 deliveries: 10 from the first peer + 1 from the second.
	delivered := 0
	deadline := time.After(5 * time.Second)
	for delivered < 11 {
		select {
		case <-got:
			delivered++
		case <-deadline:
			t.Fatalf("delivered = %d, want 11", delivered)
		}
	}
	select {
	case r := <-got:
		t.Fatalf("unexpected extra delivery: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
	t.Logf("rate limit enforced: 10 accepted, 2 rate_limited, second peer unaffected, %d delivered", delivered)
}
