// Package p2p owns the libp2p host: creation, the resource stream handler,
// and dialing a peer to send our ResourceInfo.
//
// Behavior preserved from the original runtime: listen addresses,
// protocol usage, and the human-readable receive printout.
package p2p

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ws "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"

	"distributed-ai/node/internal/protocol"
)

// New creates the libp2p host with the node's identity.
func New(priv crypto.PrivKey) (host.Host, error) {
	return NewWithAddrs(priv,
		"/ip4/0.0.0.0/tcp/4001",
		"/ip4/0.0.0.0/udp/4001/quic-v1",
	)
}

// NewWithPort is New on a custom p2p port (lets two nodes share one box;
// production default stays 4001).
func NewWithPort(priv crypto.PrivKey, port int) (host.Host, error) {
	return NewWithAddrs(priv,
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
	)
}

// Config carries the optional NAT-traversal and handler wiring for a
// node host. The zero value reproduces the historical behavior exactly
// (no relay client, no hole punching): every traversal feature is
// opt-in so existing deployments are unaffected.
type Config struct {
	// RelayAddr is the full multiaddr of a DAIN relay
	// (…/p2p/<relayPeerID>). When set, the host enables AutoRelay with
	// this static relay and hole punching (DCUtR over AutoNAT): the
	// node reserves a relay slot, advertises /p2p-circuit addresses,
	// and upgrades to direct connections whenever possible.
	RelayAddr string
	// ForcePrivate forces the AutoNAT reachability verdict to
	// "private". It exists for isolated tests (loopback hosts are
	// classified reachable and would never engage AutoRelay) and for
	// operators who KNOW the node is behind NAT. Production default:
	// false.
	ForcePrivate bool
	// DisableHolePunching turns DCUtR off while keeping AutoRelay.
	// Tests use it to observe the pure relayed path (on loopback a
	// hole punch would legitimately upgrade to direct and change the
	// connection kind mid-assertion). Production default: false —
	// nodes DO hole-punch.
	DisableHolePunching bool
	// OnResourceInfo, when set, receives (info, authenticated sender)
	// for every ACCEPTED V2 exchange. Nil keeps the print handler.
	OnResourceInfo func(info protocol.ResourceInfo, from peer.ID)
	// WSClientTLS, when set, is used for outbound WSS dials (tests use
	// it with a self-signed local relay; production relays present
	// publicly trusted certificates and need no client config).
	WSClientTLS *tls.Config
}

// NewWithAddrs is New with explicit listen addresses (used by tests to grab
// ephemeral ports; production always uses New's fixed addresses).
func NewWithAddrs(priv crypto.PrivKey, addrs ...string) (host.Host, error) {
	return NewWithConfig(priv, Config{}, addrs...)
}

// NewWithConfig is NewWithAddrs plus a Config.
func NewWithConfig(priv crypto.PrivKey, cfg Config, addrs ...string) (host.Host, error) {
	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(addrs...),
	}
	if cfg.WSClientTLS != nil {
		// Supplying ANY libp2p Transport option disables the default
		// transport fallback, so a custom WebSocket transport must be
		// registered alongside the TCP and QUIC transports this host
		// normally listens on. Otherwise even a plain /tcp listen
		// address fails with "no transport for protocol".
		opts = append(opts,
			libp2p.Transport(tcp.NewTCPTransport),
			libp2p.Transport(quic.NewTransport),
			libp2p.Transport(ws.New, ws.WithTLSClientConfig(cfg.WSClientTLS)),
		)
	}
	if cfg.RelayAddr != "" {
		relayMA, err := ma.NewMultiaddr(cfg.RelayAddr)
		if err != nil {
			return nil, fmt.Errorf("bad relay multiaddr: %w", err)
		}
		relayInfo, err := peer.AddrInfoFromP2pAddr(relayMA)
		if err != nil {
			return nil, fmt.Errorf("bad relay addr: %w", err)
		}
		opts = append(opts,
			// go-libp2p's AutoRelay defaults are hostile to a
			// single-static-relay deployment: bootDelay=3 minutes,
			// backoff=1 hour, minCandidates=4. A node would sit
			// unreachable for minutes after every start (and an hour
			// after one failed attempt). DAIN runs ONE designated
			// relay and wants reachability in seconds, so all four
			// are set deliberately.
			libp2p.EnableAutoRelayWithStaticRelays([]peer.AddrInfo{*relayInfo},
				autorelay.WithBootDelay(5*time.Second),
				autorelay.WithMinInterval(10*time.Second),
				autorelay.WithBackoff(10*time.Second),
				autorelay.WithMinCandidates(1),
				autorelay.WithNumRelays(1),
			),
		)
		if !cfg.DisableHolePunching {
			opts = append(opts, libp2p.EnableHolePunching())
		}
	}
	if cfg.ForcePrivate {
		opts = append(opts, libp2p.ForceReachabilityPrivate())
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}
	h.SetStreamHandler(protocol.ResourceProtocol, HandleResourceStream)
	h.SetStreamHandler(protocol.ResourceProtocolV2, NewResourceStreamV2Handler(cfg.OnResourceInfo))
	return h, nil
}

// ConnKind classifies a connection as "relayed" when its remote
// multiaddr traverses a circuit relay, else "direct". This is the one
// canonical classification used in logs and tests — a relayed success
// must never be reported as a direct success.
func ConnKind(c network.Conn) string {
	if c == nil {
		return "unknown"
	}
	if strings.Contains(c.RemoteMultiaddr().String(), "/p2p-circuit") {
		return "relayed"
	}
	return "direct"
}

// NewResourceStreamV2Handler builds the V2 stream handler (exported
// for tools and tests that need a custom receive callback on a host
// they construct themselves). The identity rule is enforced here,
// before any callback: the payload's self-declared PeerID must equal
// the connection's authenticated RemotePeer, or the info is rejected
// in the ACK and never delivered.
func NewResourceStreamV2Handler(onInfo func(info protocol.ResourceInfo, from peer.ID)) network.StreamHandler {
	return func(s network.Stream) {
		defer s.Close()
		remote := s.Conn().RemotePeer()
		var info protocol.ResourceInfo
		ack := protocol.ResourceAck{PeerID: remote.String()}
		if err := json.NewDecoder(s).Decode(&info); err != nil {
			ack.Error = "decode_failed"
			_ = json.NewEncoder(s).Encode(ack)
			return
		}
		if info.PeerID != remote.String() {
			ack.Error = "peer_id_mismatch"
			_ = json.NewEncoder(s).Encode(ack)
			fmt.Printf("resource v2: REJECTED info from %s claiming peer_id %s\n", remote, info.PeerID)
			return
		}
		ack.OK = true
		_ = json.NewEncoder(s).Encode(ack)
		if onInfo != nil {
			onInfo(info, remote)
		} else {
			fmt.Printf("resource v2: accepted info from %s (%s connection)\n", remote, ConnKind(s.Conn()))
		}
	}
}

// HandleResourceStream receives a peer's ResourceInfo and prints it.
// Same output format as the original runtime.
func HandleResourceStream(s network.Stream) {
	defer s.Close()

	var info protocol.ResourceInfo
	if err := json.NewDecoder(s).Decode(&info); err != nil {
		fmt.Println("Failed to read resource info:", err)
		return
	}

	fmt.Println()
	fmt.Println("================================")
	fmt.Println(" Resource info received")
	fmt.Println("================================")
	fmt.Println("Node:", info.NodeName)
	fmt.Println("Peer ID:", info.PeerID)
	fmt.Println("OS:", info.OS)
	fmt.Println("Architecture:", info.Architecture)
	fmt.Println("CPU:", info.CPUModel)
	fmt.Println("Physical cores:", info.PhysicalCores)
	fmt.Println("Logical cores:", info.LogicalCores)
	fmt.Printf("RAM: %.1f GB\n", info.RAMTotalGB)
	fmt.Println("GPU:", info.GPUModel)
	if info.GPUVendor != "" {
		fmt.Println("GPU vendor:", info.GPUVendor)
	}
	if info.GPUCores > 0 {
		fmt.Println("GPU cores:", info.GPUCores)
	}
	if info.GPUDriver != "" {
		fmt.Println("GPU driver:", info.GPUDriver)
	}
	if info.MetalSupport != "" {
		fmt.Println("Metal:", info.MetalSupport)
	}
	fmt.Printf("Storage total: %.1f GB\n", info.StorageTotalGB)
	fmt.Printf("Storage used: %.1f GB\n", info.StorageUsedGB)
	fmt.Printf("Storage free: %.1f GB\n", info.StorageFreeGB)
	fmt.Println()
	fmt.Printf("CPU share: %d%%\n", info.CPUShare)
	fmt.Printf("GPU share: %d%%\n", info.GPUShare)
	fmt.Printf("RAM share: %d%%\n", info.RAMShare)
	fmt.Printf("Storage share: %d%%\n", info.StorageShare)
	fmt.Println("================================")
}

// SendResult describes a V2-first send attempt.
type SendResult struct {
	// ACKed: the receiver verified our identity and positively
	// acknowledged the info (V2 only).
	ACKed bool
	// FellBack: the peer does not speak V2, so the info went over the
	// legacy fire-and-forget protocol. Delivery is NOT proven.
	FellBack bool
	// Kind is the connection classification: "direct" or "relayed".
	Kind string
}

// ConnectAndSendV2 dials addrStr and sends info preferring the V2
// protocol (ACK + receiver-side PeerID verification). Circuit
// addresses work as-is. If the peer answers "protocol not supported"
// the send falls back to legacy v0.4.0 and reports FellBack — never
// silently pretending the old exchange was acknowledged.
func ConnectAndSendV2(ctx context.Context, h host.Host, addrStr string, info protocol.ResourceInfo) (SendResult, error) {
	var res SendResult
	addr, err := ma.NewMultiaddr(addrStr)
	if err != nil {
		return res, fmt.Errorf("bad multiaddr: %w", err)
	}
	pi, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return res, fmt.Errorf("bad p2p addr: %w", err)
	}
	if err := h.Connect(ctx, *pi); err != nil {
		return res, fmt.Errorf("connect failed: %w", err)
	}
	// Relayed connections are TRANSIENT in libp2p: a stream over one
	// only opens when the context opts in with WithAllowLimitedConn.
	// Without it NewStream silently waits for a non-transient
	// connection until the context deadline — discovered the hard way
	// (relay_test.go reproduces both the hang and the fix).
	// (2026-10-25: WithUseTransient is deprecated in go-libp2p
	// v0.50.0; WithAllowLimitedConn sets the same context key, so
	// this is a rename-only change with identical behaviour.)
	stream, err := h.NewStream(network.WithAllowLimitedConn(ctx, string(protocol.ResourceProtocolV2)), pi.ID, protocol.ResourceProtocolV2)
	if err != nil {
		// libp2p phrases negotiation failure as "protocols not
		// supported: [<id>]" (older builds: "protocol not supported").
		if strings.Contains(err.Error(), "not supported") {
			// Legacy peer (v0.10-alpha): fire-and-forget, unacknowledged.
			if err := ConnectAndSend(ctx, h, addrStr, info); err != nil {
				return res, err
			}
			res.FellBack = true
			if conns := h.Network().ConnsToPeer(pi.ID); len(conns) > 0 {
				res.Kind = ConnKind(conns[0])
			}
			return res, nil
		}
		return res, fmt.Errorf("open stream failed: %w", err)
	}
	defer stream.Close()
	// Classify by the connection the stream actually rides — a peer
	// may hold both a relayed and a direct connection, and only the
	// stream's own transport tells the truth about this exchange.
	res.Kind = ConnKind(stream.Conn())
	if err := json.NewEncoder(stream).Encode(info); err != nil {
		return res, fmt.Errorf("send failed: %w", err)
	}
	var ack protocol.ResourceAck
	if err := json.NewDecoder(stream).Decode(&ack); err != nil {
		return res, fmt.Errorf("read ack failed: %w", err)
	}
	if !ack.OK {
		return res, fmt.Errorf("receiver rejected info: %s", ack.Error)
	}
	res.ACKed = true
	return res, nil
}

// relayPeerOfCircuitAddr extracts the relay's peer ID from a
// /p2p-circuit address (the first /p2p/ component — the relay hop
// precedes the destination's).
func relayPeerOfCircuitAddr(addr ma.Multiaddr) (peer.ID, error) {
	for _, c := range ma.Split(addr) {
		if c.Protocol().Code == ma.P_P2P {
			return peer.ID(c.Value()), nil
		}
	}
	return "", fmt.Errorf("no relay peer in circuit address")
}

// ConnectWithRetry repeats ConnectAndSendV2 with exponential backoff
// (1s doubling, capped at maxBackoff) until it succeeds or ctx ends.
// It is the --connect behavior: a node whose bootstrap/relay path is
// briefly unavailable rejoins on its own instead of giving up.
func ConnectWithRetry(ctx context.Context, h host.Host, addrStr string, info protocol.ResourceInfo, maxBackoff time.Duration, logf func(string, ...any)) (SendResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		res, err := ConnectAndSendV2(ctx, h, addrStr, info)
		if err == nil {
			logf("connect: sent resource info (acked=%v fallback=%v kind=%s) after %d attempt(s)", res.ACKed, res.FellBack, res.Kind, attempt)
			return res, nil
		}
		logf("connect: attempt %d failed: %v (retry in %s)", attempt, err, backoff)
		select {
		case <-ctx.Done():
			return SendResult{}, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// ConnectAndSend dials the peer at addrStr and sends info over the resource
// protocol. Mirrors the original main() connect+send flow.
func ConnectAndSend(ctx context.Context, h host.Host, addrStr string, info protocol.ResourceInfo) error {
	addr, err := ma.NewMultiaddr(addrStr)
	if err != nil {
		return fmt.Errorf("bad multiaddr: %w", err)
	}
	pi, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return fmt.Errorf("bad p2p addr: %w", err)
	}
	if err := h.Connect(ctx, *pi); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	stream, err := h.NewStream(ctx, pi.ID, protocol.ResourceProtocol)
	if err != nil {
		return fmt.Errorf("open stream failed: %w", err)
	}
	defer stream.Close()
	if err := json.NewEncoder(stream).Encode(info); err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	return nil
}
