# Phase A — libp2p Relay + NAT Traversal: minimal implementation plan

Status: **plan only.** No relay is built, deployed, or connected by this
document. Relay deployment and any Oracle change need separate
approval. Basis: the 2026-10-21 technical review
(`phase-a-nat-traversal-review.md` in the goal workspace), which read
the current code and the vendored go-libp2p v0.50.0 sources directly.

## Goal

Mac/Windows/Linux nodes behind NAT (including public Wi-Fi where
SSH/22 is blocked) reach the DAIN network with DAIN's own libp2p
stack: Circuit Relay v2 over TLS/WSS (port 443), AutoRelay, AutoNAT
and DCUtR hole punching — no Tailscale, no manual SSH tunnels.

## Verified starting facts (from code, not documentation)

- `node/internal/p2p/p2p.go` creates hosts with only `Identity` and
  `ListenAddrStrings`: no AutoRelay, no DCUtR, no relay service.
  Listeners are TCP 4001 + QUIC/UDP 4001 only.
- go-libp2p v0.50.0 already enables the Circuit v2 client and the
  WebSocket transport by default, and the AutoNAT v1 client is on —
  but a WSS listener is refused without TLS configuration, and relay
  *service* (for the relay node itself) is opt-in.
- Defects that block relay adoption regardless of options:
  1. `node/cmd/node/main.go` snapshots `h.Addrs()` once at startup
     into the reporter config — relay addresses learned later can
     never be advertised.
  2. ResourceInfo is fire-and-forget with no ACK, and the service does
     not check that the payload PeerID matches the connection's
     RemotePeer — any connected peer can claim any identity.
  3. `--connect` is one-shot: no retry/reconnect.
- `service/internal/p2pnet` dials nodes back over TCP/QUIC 4002;
  `protocol.ConnectPeer` mishandles `/p2p-circuit` addresses.
- Relay v2 defaults (128 reservations, 2 min / 128 KB per connection)
  are sized for signaling, not 64 MB chunks: limits must be raised on
  our relay with ACL-gated reservations.

## Minimal change set (Phase A scope only)

### A. New component: relay/bootstrap node — `node/cmd/relay` (new)
- libp2p host with `EnableRelayService` (custom `relay.Resources`:
  raised duration/data limits, reservation TTL), `EnableNATService`
  (AutoNAT server), static listen on WSS 443 + TCP/QUIC 4001.
- TLS: public certificate is required for WSS. Two options, decide
  before implementation: (a) a domain + ACME via Caddy in front
  (WS upgrade proxy), (b) libp2p WebSocket transport with a provided
  TLS config. No self-signed certs for real nodes.
- Access control: reservation ACL allowing only allowlisted PeerIDs
  (the existing node-identity registry is the allowlist source);
  relay is data-plane for circuits only — no API, no credits.
- Runs on a separate public VM (not the Oracle service VM), so the
  running `dain-svc`/`dain-node`/`llama-server` are untouched.

### B. Node host options — `node/internal/p2p/p2p.go`
- Add `EnableHolePunching()`, `EnableAutoRelayWithStaticRelays([relay])`,
  keep QUIC+TCP, add WS listener for browser-adjacent environments.
- Return the host's address stream: replace the one-time `h.Addrs()`
  snapshot with `host.Addrs()` at report time (or subscribe to
  address-change events) — fixes defect 1.

### C. Reporter + service verification — `node/internal/reporter`, `service/internal/p2pnet`
- ResourceInfo: add ACK + require payload PeerID == connection
  RemotePeer; reject mismatches (fixes the identity-claim hole).
- `--connect`: bounded retry with backoff for the bootstrap peer.
- Service dial-back: classify each node connection as direct vs
  relayed by the presence of `/p2p-circuit` in the remote multiaddr,
  and record it (this is also the log discipline for all future
  tests: a relayed success is never reported as a direct success).

### D. What is explicitly NOT in Phase A scope
- No changes to Oracle services, ports, or firewall (separate
  approval; the relay lives elsewhere).
- No chunk transfer over relay (relay limits make it
  signaling/bootstrap + small control traffic first; bulk data waits
  for direct/DCUtR paths or a later decision).
- No credit/ledger interaction. No C/D experiment features.

## Acceptance tests (isolated first, then one external node)

1. Local: relay + two nodes on one host; node B reserves via relay,
   node A reaches B through the circuit; logs show `/p2p-circuit`.
2. NAT simulation: node B behind a NAT-ed namespace (or a second
   network) — AutoNAT classifies, DCUtR attempts direct upgrade;
   outcome (direct vs relayed) recorded from multiaddrs, not assumed.
3. WSS-only egress: block all but 443 for node B; it must still join
   via the relay's WSS listener.
4. Negative: non-allowlisted PeerID reservation is refused; spoofed
   ResourceInfo PeerID is rejected by the service.
5. Reconnect: kill/restart the relay; nodes re-reserve and recover
   without process restarts.

## Decisions needed before implementation

- Which public VM hosts the relay, and under which domain (drives the
  TLS option above).
- Whether the first external test peer is the user's Mac on home NAT,
  public Wi-Fi, or both in sequence.
