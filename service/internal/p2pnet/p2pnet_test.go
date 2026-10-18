package p2pnet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
)

// testHost creates a host with its own temp HOME, so each host gets a
// distinct persistent identity. (Two hosts sharing one HOME would load the
// same identity.key and become the same peer.)
func testHost(t *testing.T) host.Host {
	t.Helper()
	h, err := newHost(t.TempDir(), "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestHostPersistentIdentity(t *testing.T) {
	home := t.TempDir()
	h1, err := newHost(home, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	id1 := h1.ID()
	h1.Close()

	h2, err := newHost(home, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if h2.ID() != id1 {
		t.Fatal("service host must keep the same Peer ID across restarts")
	}
}

func TestDialBackSucceedsLoopback(t *testing.T) {
	svc := testHost(t)
	node := testHost(t)

	var addrs []string
	for _, a := range node.Addrs() {
		addrs = append(addrs, a.String())
	}
	if len(addrs) == 0 {
		t.Fatal("node has no listen addresses")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if ok, _ := DialBack(ctx, svc, node.ID().String(), addrs, ""); !ok {
		t.Fatal("expected dial-back to succeed on loopback")
	}
}

func TestDialBackFailsUnreachable(t *testing.T) {
	svc := testHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// 192.0.2.0/24 is TEST-NET-1: unroutable by definition.
	if ok, _ := DialBack(ctx, svc, svc.ID().String(), []string{"/ip4/192.0.2.1/tcp/4001"}, ""); ok {
		t.Fatal("expected dial-back to fail for unroutable address")
	}
}

func TestDialBackSkipsUnspecified(t *testing.T) {
	svc := testHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ok, _ := DialBack(ctx, svc, svc.ID().String(), []string{"/ip4/0.0.0.0/tcp/4001"}, ""); ok {
		t.Fatal("unspecified address must not dial")
	}
}

func TestDialBackViaSourceIP(t *testing.T) {
	svc := testHost(t)
	node := testHost(t)

	// Simulate the real deployment: the node listens on 0.0.0.0 (reported
	// addrs are unspecified, hence useless), but it registered from
	// 127.0.0.1 — the source IP + p2p port must still verify it.
	var port string
	for _, a := range node.Addrs() {
		parts := strings.Split(a.String(), "/")
		for i, p := range parts {
			if p == "tcp" && i+1 < len(parts) {
				port = parts[i+1]
			}
		}
	}
	if port == "" {
		t.Fatal("no tcp port in node addrs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if ok, _ := DialBack(ctx, svc, node.ID().String(), []string{"/ip4/0.0.0.0/tcp/" + port}, "127.0.0.1"); !ok {
		t.Fatal("expected dial-back via source IP to succeed on loopback")
	}
}

func TestDialBackBadPeerID(t *testing.T) {
	svc := testHost(t)
	if ok, _ := DialBack(context.Background(), svc, "not-a-peer-id", []string{"/ip4/127.0.0.1/tcp/4001"}, ""); ok {
		t.Fatal("bad peer ID must fail")
	}
}
