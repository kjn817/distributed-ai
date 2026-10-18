package p2p

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"distributed-ai/node/internal/protocol"
)

func peerAddrInfo(t *testing.T, addrStr string) peer.AddrInfo {
	t.Helper()
	addr, err := ma.NewMultiaddr(addrStr)
	if err != nil {
		t.Fatal(err)
	}
	ai, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		t.Fatal(err)
	}
	return *ai
}

func testKey(t *testing.T) crypto.PrivKey {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// TestConnectAndSendLoopback exercises the real wire path: dial a peer over
// loopback TCP, open the resource protocol stream, send ResourceInfo, and
// decode it on the other side. This is the original main() connect+send flow.
func TestConnectAndSendLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	received := make(chan protocol.ResourceInfo, 1)
	hostB, err := NewWithAddrs(testKey(t), "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostB.Close()
	// Override the print handler with a capturing one for this test.
	hostB.SetStreamHandler(protocol.ResourceProtocol, func(s network.Stream) {
		defer s.Close()
		var info protocol.ResourceInfo
		if err := json.NewDecoder(s).Decode(&info); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		received <- info
	})

	hostA, err := NewWithAddrs(testKey(t), "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostA.Close()

	var bAddr ma.Multiaddr
	for _, a := range hostB.Addrs() {
		if _, err := a.ValueForProtocol(ma.P_TCP); err == nil {
			bAddr = a
			break
		}
	}
	if bAddr == nil {
		t.Fatal("hostB has no TCP address")
	}
	fullAddr := bAddr.Encapsulate(ma.StringCast("/p2p/" + hostB.ID().String()))

	want := protocol.ResourceInfo{
		NodeName: "test-a", PeerID: hostA.ID().String(), OS: "linux",
		CPUModel: "test-cpu", PhysicalCores: 4, LogicalCores: 8,
		CPUShare: 10, GPUShare: 10, RAMShare: 10, StorageShare: 10,
	}
	if err := ConnectAndSend(ctx, hostA, fullAddr.String(), want); err != nil {
		t.Fatalf("ConnectAndSend: %v", err)
	}

	select {
	case got := <-received:
		if got.NodeName != want.NodeName || got.PeerID != want.PeerID ||
			got.PhysicalCores != want.PhysicalCores || got.CPUShare != want.CPUShare {
			t.Fatalf("mismatch:\n got %+v\nwant %+v", got, want)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for resource info")
	}
}

func TestConnectAndSendBadAddr(t *testing.T) {
	h, err := NewWithAddrs(testKey(t), "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := ConnectAndSend(context.Background(), h, "not-a-multiaddr", protocol.ResourceInfo{}); err == nil {
		t.Fatal("expected error for bad multiaddr")
	}
}

func TestHandleResourceStreamGarbage(t *testing.T) {
	// The handler must not panic on undecodable input.
	hostA, err := NewWithAddrs(testKey(t), "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostA.Close()
	hostB, err := NewWithAddrs(testKey(t), "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var bAddr ma.Multiaddr
	for _, a := range hostB.Addrs() {
		if _, err := a.ValueForProtocol(ma.P_TCP); err == nil {
			bAddr = a
			break
		}
	}
	pi := bAddr.Encapsulate(ma.StringCast("/p2p/" + hostB.ID().String()))
	ai := peerAddrInfo(t, pi.String())
	if err := hostA.Connect(ctx, ai); err != nil {
		t.Fatal(err)
	}
	s, err := hostA.NewStream(ctx, hostB.ID(), protocol.ResourceProtocol)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Write([]byte("this is not json"))
	s.Close()
	time.Sleep(500 * time.Millisecond) // let the handler run; panic would fail the test
}
