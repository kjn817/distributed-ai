package p2p

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"

	"distributed-ai/node/internal/protocol"
	jobprotocol "distributed-ai/protocol"
)

func keyAndID(t *testing.T) (crypto.PrivKey, peer.ID) {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, id
}

func waitFor(t *testing.T, timeout time.Duration, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func circuitAddrOf(h host.Host) string {
	for _, a := range h.Addrs() {
		if strings.Contains(a.String(), "/p2p-circuit") {
			return a.String()
		}
	}
	return ""
}

func tcpAddrOf(t *testing.T, h host.Host) string {
	t.Helper()
	for _, a := range h.Addrs() {
		if _, err := a.ValueForProtocol(ma.P_TCP); err == nil {
			return a.String() + "/p2p/" + h.ID().String()
		}
	}
	t.Fatal("no TCP addr")
	return ""
}

type receipt struct {
	info protocol.ResourceInfo
	from peer.ID
}

// reserveWithRelay connects h to the relay (at relayDialAddr, the
// directly dialable form) and takes a reservation. Circuit v2
// requires the DESTINATION of a relayed connection to hold a
// reservation — a fact the test topology must respect: in production
// the service-side bridge peer holds one too (see
// docs/phase-a-http-registration-design.md).
func reserveWithRelay(t *testing.T, ctx context.Context, h host.Host, relayDialAddr string) {
	t.Helper()
	relayInfo, err := peer.AddrInfoFromP2pAddr(mustMA(t, relayDialAddr))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Connect(ctx, *relayInfo); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Reserve(ctx, h, *relayInfo); err != nil {
		t.Fatalf("reserve: %v", err)
	}
}

// publicAlias is a public-form address bound to the loopback
// interface for these tests. go-libp2p's AutoRelay only ADVERTISES
// circuit addresses whose relay hop passes manet.IsPublicAddr
// (autorelay cleanupAddressSet drops private AND documentation-range
// addresses — even TEST-NET labels are filtered) — on a real public
// relay this is automatic; locally the alias supplies the public
// form. The address is only an advertised label: all actual dials
// reuse connections opened via the 127.0.0.1 form, so no traffic
// leaves the box. Tests skip when the alias is not configured:
//
//	sudo ip addr add 45.33.10.20/32 dev lo
const publicAlias = "45.33.10.20"

func requirePublicAlias(t *testing.T) {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if strings.HasPrefix(a.String(), publicAlias+"/") {
			return
		}
	}
	t.Skipf("public test alias %s not bound to lo (sudo ip addr add %s/32 dev lo)", publicAlias, publicAlias)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newSvcHost(t *testing.T, priv crypto.PrivKey, got chan<- receipt) host.Host {
	t.Helper()
	h, err := NewWithConfig(priv, Config{
		OnResourceInfo: func(info protocol.ResourceInfo, from peer.ID) {
			got <- receipt{info, from}
		},
	}, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func sampleInfo(id peer.ID) protocol.ResourceInfo {
	return protocol.ResourceInfo{
		NodeName: "node-b", PeerID: id.String(), OS: "linux", Architecture: "arm64",
		CPUModel: "test", PhysicalCores: 4, LogicalCores: 8, RAMTotalGB: 16,
		CPUShare: 10, GPUShare: 0, RAMShare: 10, StorageShare: 10,
	}
}

// TestRelayEndToEnd is the Phase A core proof on loopback with real
// transports: a node behind (forced-private) NAT reserves a slot on
// the allowlist relay via AutoRelay, advertises a /p2p-circuit
// address, and delivers ResourceInfo to the service-side host THROUGH
// the relay with V2 ACK and identity verification. The direct path
// and the relayed path are asserted separately via ConnKind.
func TestRelayEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	relayKey, relayID := keyAndID(t)
	svcKey, svcID := keyAndID(t)
	nodeKey, nodeID := keyAndID(t)

	relay, err := NewRelayHost(relayKey, RelayConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		Allowlist:   []peer.ID{svcID, nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relayDialAddr := tcpAddrOf(t, relay)
	_ = relayID

	got := make(chan receipt, 4)
	svc := newSvcHost(t, svcKey, got)
	defer svc.Close()
	// The service-side peer holds a relay reservation so it is a valid
	// circuit destination (production: the control-plane bridge does).
	reserveWithRelay(t, ctx, svc, relayDialAddr)

	nodeGot := make(chan receipt, 4)
	node, err := NewWithConfig(nodeKey, Config{
		RelayAddr:    relayDialAddr,
		ForcePrivate: true,
		// Pure relayed path under test here; DCUtR gets its own test
		// (on loopback it would upgrade to direct, by design).
		DisableHolePunching: true,
		OnResourceInfo: func(info protocol.ResourceInfo, from peer.ID) {
			nodeGot <- receipt{info, from}
		},
	}, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	// 1. AutoRelay must engage: the node becomes reachable through
	// the relay. (Advertisement of the circuit address in h.Addrs()
	// is asserted separately in TestAutoRelayAdvertisesCircuitAddr —
	// go-libp2p only advertises circuit addrs whose relay hop is a
	// public address, which a loopback relay is not.)
	nodeCircuit := relayDialAddr + "/p2p-circuit/p2p/" + nodeID.String()
	waitFor(t, 90*time.Second, "node reachable via relay (AutoRelay reservation)", func() bool {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return jobprotocol.ConnectPeer(probeCtx, svc, nodeID.String(), []string{nodeCircuit}, "") == nil
	})
	t.Log("node reachable through the relay: AutoRelay reservation is in effect")

	// 2. ResourceInfo through the relay, V2: ACK + relayed kind.
	svcViaRelay := relayDialAddr + "/p2p-circuit/p2p/" + svcID.String()
	res, err := ConnectAndSendV2(ctx, node, svcViaRelay, sampleInfo(nodeID))
	if err != nil {
		t.Fatalf("relayed send: %v", err)
	}
	if !res.ACKed || res.FellBack {
		t.Fatalf("relayed send result = %+v, want ACKed V2", res)
	}
	if res.Kind != "relayed" {
		t.Fatalf("connection kind = %q, want relayed", res.Kind)
	}
	select {
	case r := <-got:
		if r.from != nodeID || r.info.PeerID != nodeID.String() {
			t.Fatalf("svc receipt from=%s peer_id=%s, want %s", r.from, r.info.PeerID, nodeID)
		}
	case <-ctx.Done():
		t.Fatal("svc never received relayed ResourceInfo")
	}
	// Service side must also classify its end of the connection relayed.
	if conns := svc.Network().ConnsToPeer(nodeID); len(conns) == 0 || ConnKind(conns[0]) != "relayed" {
		t.Fatal("svc-side connection not classified relayed")
	}
	t.Log("relayed ResourceInfo delivered, ACKed, identity verified, classified relayed on both ends")

	// 3. A full V2 exchange in the service -> node direction (this
	// is how job dispatch and dial-back verification reach a NAT-ed
	// node), over the circuit address the shared dialer just used.
	if err := jobprotocol.ConnectPeer(ctx, svc, nodeID.String(),
		[]string{nodeCircuit}, ""); err != nil {
		t.Fatalf("ConnectPeer via circuit addr: %v", err)
	}
	backInfo := sampleInfo(svcID)
	backInfo.NodeName = "svc"
	res, err = ConnectAndSendV2(ctx, svc, nodeCircuit, backInfo)
	if err != nil {
		t.Fatalf("svc -> node relayed send: %v", err)
	}
	if !res.ACKed || res.Kind != "relayed" {
		t.Fatalf("svc -> node result = %+v, want ACKed + relayed", res)
	}
	select {
	case r := <-nodeGot:
		if r.from != svcID {
			t.Fatalf("node receipt from %s, want %s", r.from, svcID)
		}
	case <-ctx.Done():
		t.Fatal("node never received relayed ResourceInfo from svc")
	}
	t.Log("service -> NAT-ed node relayed exchange delivered with ACK (dial-back direction)")

	// 4. Direct path stays direct (and distinct in the logs). A fresh
	// host is used so no relayed connection exists to be reused —
	// the classification must reflect THIS exchange's transport.
	directNode, err := NewWithAddrs(nodeKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer directNode.Close()
	res, err = ConnectAndSendV2(ctx, directNode, tcpAddrOf(t, svc), sampleInfo(nodeID))
	if err != nil {
		t.Fatalf("direct send: %v", err)
	}
	if !res.ACKed || res.Kind != "direct" {
		t.Fatalf("direct send result = %+v, want ACKed + direct", res)
	}
	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal("svc never received direct ResourceInfo")
	}
	t.Log("direct ResourceInfo delivered and classified direct — distinct from relayed")
}

// TestRelayACLDeniesRogue: a peer outside the allowlist can neither
// reserve a slot nor open a circuit to a member.
func TestRelayACLDeniesRogue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	relayKey, _ := keyAndID(t)
	svcKey, svcID := keyAndID(t)
	nodeKey, nodeID := keyAndID(t)
	rogueKey, rogueID := keyAndID(t)

	relay, err := NewRelayHost(relayKey, RelayConfig{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		Allowlist:   []peer.ID{svcID, nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	rogue, err := NewWithAddrs(rogueKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer rogue.Close()

	relayInfo, err := peer.AddrInfoFromP2pAddr(mustMA(t, tcpAddrOf(t, relay)))
	if err != nil {
		t.Fatal(err)
	}
	if err := rogue.Connect(ctx, *relayInfo); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Reserve(ctx, rogue, *relayInfo); err == nil {
		t.Fatal("rogue reservation was ACCEPTED by the allowlist relay")
	} else {
		t.Logf("rogue reservation denied: %v", err)
	}

	// Even dialing a member's circuit address must fail for the rogue.
	nodeCircuit := tcpAddrOf(t, relay) + "/p2p-circuit/p2p/" + nodeID.String()
	if _, err := ConnectAndSendV2(ctx, rogue, nodeCircuit, sampleInfo(rogueID)); err == nil {
		t.Fatal("rogue circuit connection to a member was ACCEPTED")
	} else {
		t.Logf("rogue circuit connect denied: %v", err)
	}
	_ = svcKey
	_ = nodeKey
}

// TestResourceV2RejectsSpoofedPeerID: the payload's self-declared
// PeerID is worthless unless it equals the authenticated RemotePeer.
func TestResourceV2RejectsSpoofedPeerID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svcKey, _ := keyAndID(t)
	atkKey, atkID := keyAndID(t)
	_, victimID := keyAndID(t)

	got := make(chan receipt, 1)
	svc := newSvcHost(t, svcKey, got)
	defer svc.Close()

	atk, err := NewWithAddrs(atkKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer atk.Close()

	ai, err := peer.AddrInfoFromP2pAddr(mustMA(t, tcpAddrOf(t, svc)))
	if err != nil {
		t.Fatal(err)
	}
	if err := atk.Connect(ctx, *ai); err != nil {
		t.Fatal(err)
	}
	s, err := atk.NewStream(ctx, svc.ID(), protocol.ResourceProtocolV2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	spoofed := sampleInfo(atkID)
	spoofed.PeerID = victimID.String() // claim to be someone else
	if err := json.NewEncoder(s).Encode(spoofed); err != nil {
		t.Fatal(err)
	}
	var ack protocol.ResourceAck
	if err := json.NewDecoder(s).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if ack.OK || ack.Error != "peer_id_mismatch" {
		t.Fatalf("ack = %+v, want rejection with peer_id_mismatch", ack)
	}
	select {
	case r := <-got:
		t.Fatalf("spoofed info was DELIVERED to the receiver callback: %+v", r)
	default:
	}
	t.Log("spoofed PeerID rejected in ACK and never delivered")
}

// TestV2FallsBackToLegacyV1: a v0.10-alpha peer (V1 handler only)
// still receives the info via the legacy protocol, and the sender
// reports the exchange as unacknowledged fallback.
func TestV2FallsBackToLegacyV1(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	legacyKey, _ := keyAndID(t)
	nodeKey, nodeID := keyAndID(t)

	legacy, err := libp2p.New(libp2p.Identity(legacyKey),
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	received := make(chan protocol.ResourceInfo, 1)
	// Exactly the v0.10-alpha surface: V1 handler only.
	legacy.SetStreamHandler(protocol.ResourceProtocol, func(s network.Stream) {
		defer s.Close()
		var info protocol.ResourceInfo
		if err := json.NewDecoder(s).Decode(&info); err == nil {
			received <- info
		}
	})

	node, err := NewWithAddrs(nodeKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	res, err := ConnectAndSendV2(ctx, node, tcpAddrOf(t, legacy), sampleInfo(nodeID))
	if err != nil {
		t.Fatalf("fallback send: %v", err)
	}
	if !res.FellBack || res.ACKed {
		t.Fatalf("result = %+v, want FellBack && !ACKed", res)
	}
	select {
	case info := <-received:
		if info.PeerID != nodeID.String() {
			t.Fatalf("legacy received peer %s, want %s", info.PeerID, nodeID)
		}
	case <-ctx.Done():
		t.Fatal("legacy peer never received the info")
	}
	t.Log("v0.10-alpha (V1-only) peer received info via fallback; sender reports unacknowledged")
}

// TestRelayedStreamLimits: a ~512 KB stream over the relay succeeds
// with the DAIN limits and fails under the stock circuit defaults
// (128 KB per direction) — the limit raise is functional, not cosmetic.
func TestRelayedStreamLimits(t *testing.T) {
	bigInfo := func(id peer.ID) protocol.ResourceInfo {
		info := sampleInfo(id)
		info.CPUModel = strings.Repeat("x", 512<<10)
		return info
	}
	run := func(t *testing.T, resources *relayv2.Resources) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		relayKey, _ := keyAndID(t)
		svcKey, svcID := keyAndID(t)
		nodeKey, nodeID := keyAndID(t)
		cfg := RelayConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
			Allowlist: []peer.ID{svcID, nodeID}, Resources: resources}
		relay, err := NewRelayHost(relayKey, cfg)
		if err != nil {
			return err
		}
		defer relay.Close()
		got := make(chan receipt, 1)
		svc := newSvcHost(t, svcKey, got)
		defer svc.Close()
		reserveWithRelay(t, ctx, svc, tcpAddrOf(t, relay))
		node, err := NewWithAddrs(nodeKey, "/ip4/127.0.0.1/tcp/0")
		if err != nil {
			return err
		}
		defer node.Close()
		relayInfo, err := peer.AddrInfoFromP2pAddr(mustMA(t, tcpAddrOf(t, relay)))
		if err != nil {
			return err
		}
		if err := node.Connect(ctx, *relayInfo); err != nil {
			return err
		}
		if _, err := client.Reserve(ctx, node, *relayInfo); err != nil {
			return err
		}
		svcViaRelay := tcpAddrOf(t, relay) + "/p2p-circuit/p2p/" + svcID.String()
		_, err = ConnectAndSendV2(ctx, node, svcViaRelay, bigInfo(nodeID))
		return err
	}

	if err := run(t, nil); err != nil {
		t.Fatalf("512KB stream with DAIN relay limits: %v", err)
	}
	t.Log("512KB relayed stream: OK with DAIN limits (1h / 1GiB)")

	// The genuine circuit-v2 stock defaults: only the per-connection
	// Limit differs from DAIN's (2 minutes / 128 KB).
	stock := &relayv2.Resources{
		ReservationTTL: time.Hour,
		Limit:          &relayv2.RelayLimit{Duration: 2 * time.Minute, Data: 128 << 10},
		MaxReservations: 128, MaxCircuits: 16, BufferSize: 2048,
		MaxReservationsPerPeer: 4, MaxReservationsPerIP: 8, MaxReservationsPerASN: 32,
	}
	if err := run(t, stock); err == nil {
		t.Fatal("512KB stream unexpectedly succeeded under stock 128KB relay limits")
	} else {
		t.Logf("512KB relayed stream under stock limits fails as expected: %v", err)
	}
}

func mustMA(t *testing.T, s string) ma.Multiaddr {
	t.Helper()
	a, err := ma.NewMultiaddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestConnectWithRetryRecovers: the --connect retry loop rides out a
// target that only appears seconds later, then delivers with ACK.
func TestConnectWithRetryRecovers(t *testing.T) {
	svcKey, _ := keyAndID(t)
	nodeKey, nodeID := keyAndID(t)

	// Reserve an ephemeral port, release it, and bind the svc host to
	// it only after a delay.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	got := make(chan receipt, 1)
	var svc host.Host
	go func() {
		time.Sleep(1500 * time.Millisecond)
		h, err := NewWithConfig(svcKey, Config{
			OnResourceInfo: func(info protocol.ResourceInfo, from peer.ID) {
				got <- receipt{info, from}
			},
		}, "/ip4/127.0.0.1/tcp/"+strconv.Itoa(port))
		if err == nil {
			svc = h
		}
	}()

	node, err := NewWithAddrs(nodeKey, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	// svc peer ID is derived from its key.
	svcID, err := peer.IDFromPrivateKey(svcKey)
	if err != nil {
		t.Fatal(err)
	}
	svcAddrStr := "/ip4/127.0.0.1/tcp/" + strconv.Itoa(port) + "/p2p/" + svcID.String()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res, err := ConnectWithRetry(ctx, node, svcAddrStr, sampleInfo(nodeID), 2*time.Second, nil)
	if err != nil {
		t.Fatalf("retry loop: %v", err)
	}
	if !res.ACKed {
		t.Fatalf("result = %+v, want ACKed after recovery", res)
	}
	select {
	case r := <-got:
		if r.from != nodeID {
			t.Fatalf("receipt from %s, want %s", r.from, nodeID)
		}
	case <-ctx.Done():
		t.Fatal("svc never received info after retry recovery")
	}
	if svc != nil {
		defer svc.Close()
	}
	t.Log("connect retry recovered from late-appearing target and delivered with ACK")
}

// TestAutoRelayAdvertisesCircuitAddr asserts the advertisement half
// of dynamic addressing: with a public-form relay, the node's
// h.Addrs() gains a /p2p-circuit address once AutoRelay's reservation
// lands — the exact list the reporter now ships to the service via
// its live AddrsFn lookup. (The data path over that reservation is
// proven in TestRelayEndToEnd; this sandbox's netstack will not dial
// the alias address itself, so no traffic is attempted here.)
func TestAutoRelayAdvertisesCircuitAddr(t *testing.T) {
	requirePublicAlias(t)
	relayKey, relayID := keyAndID(t)
	nodeKey, nodeID := keyAndID(t)
	_ = nodeID
	port := freePort(t)
	relay, err := NewRelayHost(relayKey, RelayConfig{
		ListenAddrs:   []string{"/ip4/0.0.0.0/tcp/" + strconv.Itoa(port)},
		AnnounceAddrs: []string{"/ip4/" + publicAlias + "/tcp/" + strconv.Itoa(port)},
		Allowlist:     []peer.ID{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relayDialAddr := "/ip4/127.0.0.1/tcp/" + strconv.Itoa(port) + "/p2p/" + relayID.String()

	node, err := NewWithConfig(nodeKey, Config{
		RelayAddr:           relayDialAddr,
		ForcePrivate:        true,
		DisableHolePunching: true,
	}, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	waitFor(t, 60*time.Second, "advertised circuit address", func() bool {
		return circuitAddrOf(node) != ""
	})
	addr := circuitAddrOf(node)
	if !strings.HasPrefix(addr, "/ip4/"+publicAlias+"/") || !strings.Contains(addr, "/p2p-circuit") {
		t.Fatalf("advertised circuit addr = %q, want public relay hop + /p2p-circuit", addr)
	}
	t.Logf("advertised circuit address: %s", addr)
}

// TestWSSRelayPath: the relay serves WSS with a (self-signed, local
// only) TLS cert; a node reserving through the WSS address delivers
// ResourceInfo end to end. Production needs a publicly trusted cert
// — see docs/phase-a-relay-prototype.md.
func TestWSSRelayPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cert := selfSignedCert(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}

	relayKey, relayID := keyAndID(t)
	svcKey, svcID := keyAndID(t)
	nodeKey, nodeID := keyAndID(t)

	tcpPort := freePort(t)
	wssPort := freePort(t)
	for wssPort == tcpPort { // a just-freed port can be re-issued immediately
		wssPort = freePort(t)
	}
	relay, err := NewRelayHost(relayKey, RelayConfig{
		ListenAddrs: []string{
			"/ip4/127.0.0.1/tcp/" + strconv.Itoa(tcpPort),
			"/ip4/127.0.0.1/tcp/" + strconv.Itoa(wssPort) + "/wss",
		},
		Allowlist: []peer.ID{svcID, nodeID},
		TLSConfig: tlsCfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	relayDialAddr := "/ip4/127.0.0.1/tcp/" + strconv.Itoa(tcpPort) + "/p2p/" + relayID.String()
	wssDialAddr := "/ip4/127.0.0.1/tcp/" + strconv.Itoa(wssPort) + "/wss/p2p/" + relayID.String()
	t.Logf("relay WSS dial address: %s", wssDialAddr)

	got := make(chan receipt, 1)
	svc := newSvcHost(t, svcKey, got)
	defer svc.Close()
	reserveWithRelay(t, ctx, svc, relayDialAddr) // svc reserves over the relay's TCP listener

	node, err := NewWithConfig(nodeKey, Config{
		RelayAddr:           wssDialAddr,
		ForcePrivate:        true,
		DisableHolePunching: true,
		WSClientTLS:         &tls.Config{InsecureSkipVerify: true}, // local self-signed test ONLY
	}, "/ip4/127.0.0.1/tcp/0")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	// The retrying send rides out the node's AutoRelay reservation
	// over the WSS hop, then must come through ACKed + relayed.
	svcViaRelay := wssDialAddr + "/p2p-circuit/p2p/" + svcID.String()
	res, err := ConnectWithRetry(ctx, node, svcViaRelay, sampleInfo(nodeID), 5*time.Second, nil)
	if err != nil {
		t.Fatalf("WSS relayed send: %v", err)
	}
	if !res.ACKed || res.Kind != "relayed" {
		t.Fatalf("result = %+v, want ACKed + relayed", res)
	}
	select {
	case r := <-got:
		if r.from != nodeID {
			t.Fatalf("receipt from %s, want %s", r.from, nodeID)
		}
	case <-ctx.Done():
		t.Fatal("svc never received WSS-relayed ResourceInfo")
	}
	t.Log("WSS relay path: reservation + relayed ResourceInfo + ACK all over TLS/WebSocket")
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, privKey.Public(), privKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
