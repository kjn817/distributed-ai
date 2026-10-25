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
// circuits may only terminate at members.
//
// Default deny (2026-10-25): an empty allowlist denies everyone.
// Allow-all exists only as an explicit, local-test-only opt-in
// (RelayConfig.AllowAll) and must never be used on a public relay.
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
	// Allowlist of member Peer IDs. Empty means DENY ALL unless
	// AllowAll is set (see below).
	Allowlist []peer.ID
	// AllowAll explicitly opts into allow-all mode. It exists only for
	// isolated local experiments; a public relay must never set it.
	// Default: false (default deny).
	AllowAll bool
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
// raises them deliberately — but only as far as control traffic needs.
// Per-circuit data is 16 MiB (was 1 GiB before the 2026-10-25 security
// review: 1 GiB per circuit let one peer burn 32 GiB across the 32
// allowed circuits). 16 MiB still covers ResourceInfo (a few KB) and
// the 512 KiB stream probe with wide margin. Bulk chunk transfer over
// relay is NOT authorized by these numbers — that is a separate policy
// decision (see docs/phase-a-relay-minimal-plan.md).
//
// Structural server-wide cap implied by these numbers:
// MaxCircuits (32) x per-circuit Data (16 MiB) = 512 MiB of relayed
// data at most. There is no separate bandwidth meter; the cap is
// enforced by the circuit count and the per-circuit limit together.
func defaultRelayResources() relayv2.Resources {
	return relayv2.Resources{
		ReservationTTL: time.Hour,
		Limit: &relayv2.RelayLimit{
			Duration: time.Hour,
			Data:     16 << 20,
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
	// Default deny: an empty allowlist must never silently become an
	// open relay. The caller has to opt in explicitly (AllowAll) for
	// isolated local experiments, and the CLI refuses that on a public
	// host unless --allow-all is passed.
	if len(cfg.Allowlist) == 0 && !cfg.AllowAll {
		return nil, fmt.Errorf("relay allowlist is empty: refusing to start an open relay (pass AllowAll explicitly for isolated local tests only)")
	}
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
