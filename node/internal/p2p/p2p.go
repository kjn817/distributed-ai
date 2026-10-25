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
	"io"
	"strings"
	"sync"
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
	// V1 and V2 share one rate limiter per host: an attacker must not
	// be able to dodge the V2 limit by spamming the legacy protocol.
	v1Handler, v2Handler := newResourceHandlers(cfg.OnResourceInfo)
	h.SetStreamHandler(protocol.ResourceProtocol, v1Handler)
	h.SetStreamHandler(protocol.ResourceProtocolV2, v2Handler)
	return h, nil
}

// resourceRateLimiter is a per-peer sliding-window limiter for
// ResourceInfo streams. Legitimate traffic is one message per connect
// plus reconnects — a handful per minute at most. Anything above the
// limit is abnormal and is rejected + logged, never delivered.
type resourceRateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[peer.ID][]time.Time
}

func newResourceRateLimiter(limit int, window time.Duration) *resourceRateLimiter {
	return &resourceRateLimiter{limit: limit, window: window, hits: make(map[peer.ID][]time.Time)}
}

// resourceRateLimit is the default: 10 ResourceInfo messages per peer
// per minute, across V1 and V2 combined (shared limiter).
const resourceRateLimit = 10
const resourceRateWindow = time.Minute

func (l *resourceRateLimiter) Allow(id peer.ID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	hits := l.hits[id]
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[id] = kept
		return false
	}
	l.hits[id] = append(kept, now)
	return true
}

// countReader counts the bytes actually pulled from the stream.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// decodeCapped decodes ONE JSON value from r with a hard size cap.
// It returns tooLarge=true when the peer sent more than maxBytes —
// the caller must reject, never trust a truncated value.
//
// NOTE: io.ReadAll must never be used here. A libp2p stream stays open
// while the sender waits for the ACK, so ReadAll would wait for an EOF
// that only arrives after the reply — a deadlock the receiver created
// for itself (found by the 2026-10-25 rate-limit test, which hung for
// the full 10-minute test timeout on this exact bug). json.Decoder
// reads exactly one value and returns, which is the framing.
func decodeCapped(r io.Reader, maxBytes int, v any) (bool, error) {
	cr := &countReader{r: r}
	err := json.NewDecoder(io.LimitReader(cr, int64(maxBytes)+1)).Decode(v)
	if cr.n > int64(maxBytes) {
		return true, nil
	}
	return false, err
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

// decodeResourceInfo reads one ResourceInfo with the hard size cap.
// It returns tooLarge=true when the peer exceeded MaxResourceInfoBytes;
// the caller must reject, never decode a truncated prefix.
func decodeResourceInfo(s network.Stream) (protocol.ResourceInfo, bool, error) {
	var info protocol.ResourceInfo
	tooLarge, err := decodeCapped(s, protocol.MaxResourceInfoBytes, &info)
	if err != nil {
		return info, false, err
	}
	if tooLarge {
		return info, true, nil
	}
	return info, false, nil
}

// newResourceHandlers builds the V1+V2 stream handlers for one host,
// sharing a single per-peer rate limiter: an attacker must not be able
// to dodge the V2 limit by spamming the legacy protocol. Enforcement
// order on every stream is: rate limit, then size cap, then (V2 only)
// the identity rule. Every rejection is logged with the authenticated
// RemotePeer and the reason; rejected info is never delivered.
func newResourceHandlers(onInfo func(info protocol.ResourceInfo, from peer.ID)) (network.StreamHandler, network.StreamHandler) {
	limiter := newResourceRateLimiter(resourceRateLimit, resourceRateWindow)
	v2 := func(s network.Stream) {
		defer s.Close()
		remote := s.Conn().RemotePeer()
		ack := protocol.ResourceAck{PeerID: remote.String()}
		reject := func(reason string) {
			ack.Error = reason
			_ = json.NewEncoder(s).Encode(ack)
			fmt.Printf("resource v2: REJECTED info from %s: %s\n", remote, reason)
		}
		if !limiter.Allow(remote) {
			reject("rate_limited")
			return
		}
		info, tooLarge, err := decodeResourceInfo(s)
		if tooLarge {
			reject("message_too_large")
			return
		}
		if err != nil {
			reject("decode_failed")
			return
		}
		if info.PeerID != remote.String() {
			reject("peer_id_mismatch")
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
	v1 := func(s network.Stream) {
		defer s.Close()
		remote := s.Conn().RemotePeer()
		if !limiter.Allow(remote) {
			fmt.Printf("resource v1: REJECTED info from %s: rate_limited\n", remote)
			return
		}
		info, tooLarge, err := decodeResourceInfo(s)
		if tooLarge {
			fmt.Printf("resource v1: REJECTED info from %s: message_too_large\n", remote)
			return
		}
		if err != nil {
			fmt.Println("Failed to read resource info:", err)
			return
		}
		printResourceInfo(info)
	}
	return v1, v2
}

// NewResourceStreamV2Handler builds the V2 stream handler (exported
// for tools and tests that need a custom receive callback on a host
// they construct themselves). The identity rule is enforced here,
// before any callback: the payload's self-declared PeerID must equal
// the connection's authenticated RemotePeer, or the info is rejected
// in the ACK and never delivered. It carries its own rate limiter;
// hosts built by NewWithConfig share one limiter across V1+V2 instead.
func NewResourceStreamV2Handler(onInfo func(info protocol.ResourceInfo, from peer.ID)) network.StreamHandler {
	_, v2 := newResourceHandlers(onInfo)
	return v2
}

// HandleResourceStream receives a peer's ResourceInfo and prints it.
// Same output format as the original runtime.
//
// Deprecated: kept only for existing external callers. It applies the
// message size cap but has no rate limiter, because it carries no
// per-host state. Hosts built by NewWithConfig do not use it — they use
// newResourceHandlers, which rate-limits V1 and V2 together.
func HandleResourceStream(s network.Stream) {
	defer s.Close()
	info, tooLarge, err := decodeResourceInfo(s)
	if tooLarge {
		fmt.Printf("resource v1: REJECTED info from %s: message_too_large\n", s.Conn().RemotePeer())
		return
	}
	if err != nil {
		fmt.Println("Failed to read resource info:", err)
		return
	}
	printResourceInfo(info)
}

func printResourceInfo(info protocol.ResourceInfo) {
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
	ackTooLarge, err := decodeCapped(stream, protocol.MaxResourceAckBytes, &ack)
	if err != nil {
		return res, fmt.Errorf("read ack failed: %w", err)
	}
	if ackTooLarge {
		return res, fmt.Errorf("read ack failed: ack exceeds %d bytes", protocol.MaxResourceAckBytes)
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
