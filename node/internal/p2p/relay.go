package p2p

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ws "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"
)

// AllowlistACL gates relay access by Peer ID. The DAIN relay is not a
// public relay: only known network members may reserve slots, and
// circuits may only terminate at members. An EMPTY allowlist allows
// everyone — that mode exists for isolated local experiments and must
// never be used on a public relay.
type AllowlistACL struct {
	allowed map[peer.ID]bool
}

// NewAllowlistACL builds an ACL from peer IDs. No IDs = allow all.
func NewAllowlistACL(ids []peer.ID) *AllowlistACL {
	m := make(map[peer.ID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return &AllowlistACL{allowed: m}
}

// Enforced reports whether the allowlist actually restricts anyone.
func (a *AllowlistACL) Enforced() bool { return len(a.allowed) > 0 }

func (a *AllowlistACL) AllowReserve(p peer.ID, _ ma.Multiaddr) bool {
	return !a.Enforced() || a.allowed[p]
}

func (a *AllowlistACL) AllowConnect(src peer.ID, _ ma.Multiaddr, dest peer.ID) bool {
	if !a.Enforced() {
		return true
	}
	return a.allowed[src] && a.allowed[dest]
}

// RelayConfig configures the DAIN relay/bootstrap host.
type RelayConfig struct {
	// ListenAddrs for the relay host, e.g.
	// /ip4/0.0.0.0/tcp/4001, /ip4/0.0.0.0/udp/4001/quic-v1, and for WSS
	// /ip4/0.0.0.0/tcp/443/wss (requires TLSConfig).
	ListenAddrs []string
	// AnnounceAddrs, when set, overrides the addresses the relay
	// advertises (its public DNS/IP form in production).
	AnnounceAddrs []string
	// Allowlist of member Peer IDs (empty = allow all, local test only).
	Allowlist []peer.ID
	// TLSConfig enables WSS listeners (cert from a file pair in the
	// caller, or a self-signed test config). Nil = no WSS.
	TLSConfig *tls.Config
	// Resources overrides the relay limits (nil = DAIN defaults).
	// Tests use it to prove both the raised production limits and
	// what the stock circuit defaults would do to a larger stream.
	Resources *relayv2.Resources
}

// Default relay limits. The circuit-v2 defaults (2 minutes, 128 KB
// per connection) are sized for signaling and would reset a
// ResourceInfo stream mid-flight on a slow path, so the DAIN relay
// raises them deliberately: 1 hour and 1 GiB per relayed connection
// are generous for control traffic and small streams. Bulk chunk
// transfer over relay is NOT authorized by these numbers — that is a
// separate policy decision (see docs/phase-a-relay-minimal-plan.md).
func defaultRelayResources() relayv2.Resources {
	return relayv2.Resources{
		ReservationTTL: time.Hour,
		Limit: &relayv2.RelayLimit{
			Duration: time.Hour,
			Data:     1 << 30,
		},
		MaxReservations:        512,
		MaxCircuits:            32,
		BufferSize:             4096,
		MaxReservationsPerPeer: 4,
		MaxReservationsPerIP:   64,
	}
}

// NewRelayHost builds a host that serves Circuit Relay v2 and AutoNAT
// for the network. It speaks no DAIN application protocol and holds no
// service state: it is pure connectivity infrastructure.
func NewRelayHost(priv crypto.PrivKey, cfg RelayConfig) (host.Host, error) {
	acl := NewAllowlistACL(cfg.Allowlist)
	resources := defaultRelayResources()
	if cfg.Resources != nil {
		resources = *cfg.Resources
	}
	relayOpts := []relayv2.Option{relayv2.WithResources(resources)}
	if acl.Enforced() {
		relayOpts = append(relayOpts, relayv2.WithACL(acl))
	}
	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
		libp2p.EnableRelayService(relayOpts...),
		libp2p.EnableNATService(),
		// go-libp2p only activates the relay service on hosts it deems
		// publicly reachable. A relay is public infrastructure BY
		// DEFINITION, so the operator asserts reachability here —
		// without this, the hop protocol is never even registered
		// (reservations fail with "protocols not supported:
		// /libp2p/circuit/relay/0.2.0/hop"), including on real NAT-ed
		// relay hosts whose AutoNAT verdict lags.
		libp2p.ForceReachabilityPublic(),
	}
	if len(cfg.AnnounceAddrs) > 0 {
		announced := make([]ma.Multiaddr, 0, len(cfg.AnnounceAddrs))
		for _, s := range cfg.AnnounceAddrs {
			a, err := ma.NewMultiaddr(s)
			if err != nil {
				return nil, fmt.Errorf("bad announce addr %q: %w", s, err)
			}
			announced = append(announced, a)
		}
		opts = append(opts, libp2p.AddrsFactory(func([]ma.Multiaddr) []ma.Multiaddr {
			return announced
		}))
	}
	if cfg.TLSConfig != nil {
		// Supplying ANY libp2p Transport option disables the default
		// transport fallback in go-libp2p v0.50. A custom WSS transport
		// therefore has to be registered alongside the relay's normal
		// TCP and QUIC transports; on its own, only the /wss listener
		// survives and plain /tcp addresses silently never bind.
		opts = append(opts,
			libp2p.Transport(tcp.NewTCPTransport),
			libp2p.Transport(quic.NewTransport),
			libp2p.Transport(ws.New, ws.WithTLSConfig(cfg.TLSConfig)),
		)
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("relay host: %w", err)
	}
	return h, nil
}
