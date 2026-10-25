# Phase A — DAIN libp2p Relay prototype: measured results

Status: **working local prototype on `experiment/phase-a-relay`.**
Nothing here is deployed. No Oracle process was restarted or replaced,
no public port was opened, no Cloudflare setting changed, and `main`
was not merged.

The prototype uses go-libp2p v0.50.0. It proves DAIN's own Circuit
Relay v2 data path for ResourceInfo in isolated local topologies,
including separate OS processes. It does **not** prove WAN NAT
traversal, DCUtR upgrade, or Mac↔Oracle connectivity; those still need
an external-node test.

## What was implemented

### Relay / bootstrap process — `node/cmd/relay`

- Circuit Relay v2 service with DAIN limits: reservation TTL 1 hour,
  per-circuit limit 1 hour / 1 GiB, 512 reservations. The stock limits
  (2 minutes / 128 KiB) are for signalling and are tested separately
  below.
- AutoNAT service so clients can obtain reachability information.
- Peer ID allowlist ACL for both reservation and circuit connection.
  An empty allowlist allows everyone and the binary prints a warning;
  that mode is for local tests only.
- Persistent relay identity under a data directory (default
  `~/.distributed-ai-relay`), so a restart keeps the same Peer ID.
- Plain TCP/QUIC listeners and an optional WSS listener from an
  operator-supplied TLS certificate/key pair. WSS without TLS is
  refused by the CLI.

### Node host — `node/internal/p2p`

- Opt-in static AutoRelay configuration (`Config.RelayAddr`) with
  DAIN-specific timing: 5 s boot delay, 10 s minimum interval, 10 s
  backoff, one candidate / one relay. The go-libp2p defaults (3 minute
  boot delay, 1 hour backoff, four candidates) would leave a node
  unreachable for minutes in DAIN's single-designated-relay design.
- Hole punching (DCUtR) enabled with the relay configuration unless a
  test explicitly disables it to isolate the relayed path.
- `/p2p-circuit` addresses are dialed as complete addresses.
  `protocol.ConnectPeer` validates the destination Peer ID embedded in
  a circuit address and does not reuse the relay's TCP port as if it
  belonged to the destination.
- Streams over relayed connections use `network.WithUseTransient`.
  This is mandatory: without it, `NewStream` waits for a
  non-transient connection until the context expires, even though the
  circuit connection and identify exchange are healthy.
- Connection classification is taken from the connection that the
  stream actually rides: any `/p2p-circuit` remote address is
  `relayed`; otherwise `direct`.
- `--connect` behavior is available as `ConnectWithRetry` with
  exponential backoff (1 s doubling, caller-capped).

### ResourceInfo V2 — `/distributed-ai/resource/0.5.0`

- The deployed V1 protocol (`/distributed-ai/resource/0.4.0`) is
  unchanged.
- V2 adds a receiver ACK. The receiver accepts the information only
  when `info.PeerID` equals the stream's authenticated `RemotePeer`;
  a mismatch is rejected in the ACK (`peer_id_mismatch`) and never
  reaches the receive callback.
- `ConnectAndSendV2` reports `ACKed`, `FellBack`, and `Kind`
  separately. A V1-only peer receives the information by fallback and
  the result is explicitly unacknowledged.

### Dynamic reporting

- `node/internal/reporter.Config.AddrsFn` supplies current host
  addresses at registration and re-registration time.
- `node/cmd/node --relay <multiaddr>` wires the relay configuration
  and passes a live `host.Addrs()` closure to the reporter, replacing
  the startup snapshot that could never contain a relay reservation
  learned later.

## Measured locally

Canonical evidence: `docs/phase-a-relay-test-output-20261021.txt`
(`go test -count=1 -v ./node/internal/p2p/`). All 12 package tests
passed in that run.

| Required behavior | Evidence |
|---|---|
| Relay reservation succeeds | `TestRelayEndToEnd`, `TestAutoRelayAdvertisesCircuitAddr`, process smoke |
| Real relayed connection | `TestRelayEndToEnd`: send result classified `relayed`; receipt connection also classified `relayed` |
| ResourceInfo delivery | In-process E2E in both directions, plus `TestRelayProcessSmoke` with separate `dain-relay`, `relay-smoke svc`, and `relay-smoke node` processes |
| ACK + Peer ID match | V2 ACK returned; receiver callback receives the authenticated sender; `TestResourceV2RejectsSpoofedPeerID` rejects a false payload ID and does not deliver it |
| Unauthorized peer rejected | `TestRelayACLDeniesRogue`: reservation fails with circuit `PERMISSION_DENIED`; circuit connection is also denied. The process smoke repeats this against the real relay binary |
| Relay restart recovery | `TestRelayProcessSmoke`: relay process killed and restarted with the same data directory/port/Peer ID. New attempts first receive `NO_RESERVATION`, then succeed on attempt 6 with `acked=true kind=relayed` after the destination re-reserves |
| Direct vs relayed distinction | `TestRelayEndToEnd` uses a fresh direct-only host for the direct case and asserts `kind=direct`; relayed cases assert `kind=relayed` from the stream's own connection |
| Relay limits | `TestRelayedStreamLimits`: a 512 KiB stream succeeds with DAIN limits and fails under stock circuit limits |
| Late peer recovery | `TestConnectWithRetryRecovers`: retrying send succeeds when the target appears later |
| Circuit advertisement | A public-form circuit address appears in `h.Addrs()` once AutoRelay has a reservation |

The process smoke is the deployment-shaped check: three separate
binaries/processes, both ResourceInfo directions, an allowlist rogue,
and a relay restart. Its passing lines include:

```text
SMOKE: SENT acked=true fallback=false kind=relayed
node -> relay -> svc: ResourceInfo delivered, V2 ACK returned, kind=relayed
SMOKE: REPLIED acked=true kind=relayed
svc -> relay -> node: reply delivered with ACK
rogue reservation denied by the allowlist relay
relay restart: peers recovered, ResourceInfo delivered with ACK again
--- PASS: TestRelayProcessSmoke
```

One local-test caveat: `TestAutoRelayAdvertisesCircuitAddr` needs a
public-form loopback alias (`45.33.10.20/32`) because go-libp2p's
AutoRelay address cleanup discards private/documentation addresses.
Without the alias that single test skips; the data-path tests use
loopback relay addresses and do not depend on it.

## WSS/TLS result

`TestWSSRelayPath` passed with a self-signed certificate in the
isolated test:

- relay served plain TCP and `/wss` simultaneously;
- the destination reserved through the relay;
- a node used the WSS relay address as its AutoRelay hop;
- ResourceInfo crossed the circuit and returned a V2 ACK, classified
  `relayed`.

A library behavior worth recording: in go-libp2p v0.50, supplying any
custom `Transport` option disables the default transport fallback.
Both the relay host and a node with a custom WSS client TLS config
therefore register TCP and QUIC explicitly beside the customized
WebSocket transport. Without that, plain TCP listeners fail with
“no transport for protocol”.

Self-signed WSS is **not** the production configuration. Production
requires:

- a dedicated public relay host (not the Oracle service VM);
- a stable domain whose DNS name is used in the WSS multiaddr;
- a publicly trusted certificate with that domain in its SAN, supplied
  to `dain-relay --wss-port 443 --tls-cert … --tls-key …` (or a
  separately approved TLS terminator design);
- a non-empty member allowlist;
- normal process supervision, log review, and relay resource/rate
  monitoring;
- distribution of the relay's full `/dns4/<domain>/tcp/443/wss/p2p/<id>`
  address to nodes.

No public domain, certificate, host or port was created in this round.

## What is not proven

- **WAN / real NAT behavior:** all data-path evidence is local
  loopback. It must not be quoted as Mac↔Oracle evidence.
- **DCUtR upgrade:** hole punching is wired, but the local tests
  deliberately disable it to observe the pure relayed path. A direct
  upgrade on a real NAT remains unmeasured.
- **Public Wi-Fi / 443-only egress:** WSS works locally with a
  self-signed cert; a trusted-cert, real-domain, 443-only test is
  still outstanding.
- **HTTP service registration through the relay:** designed in
  `docs/phase-a-http-registration-design.md`, not implemented or
  deployed. ResourceInfo success does not register a node with the
  HTTP service.
- **Bulk/chunk transfer over relay:** not tested as a DAIN chunk
  flow and not authorized by this prototype. The 512 KiB test only
  establishes relay stream-limit behavior.
- **Literal v0.10-alpha binary interop:** V1 wire compatibility and
  fallback were tested against a V1-only host implementing the
  deployed protocol. The archived release binary itself was not run
  in this round.

## v0.10-alpha reuse boundary

Reusable unchanged:

- V1 ResourceInfo wire protocol and payload format.
- Existing nodes as V1 peers: an updated sender can still deliver
  ResourceInfo to them, reported as fallback/unacknowledged.
- Existing service and inference processes; this branch does not
  require changing them for the relay prototype.

Requires the new code/binaries:

- `dain-relay` itself.
- A node that reserves through AutoRelay, advertises circuit
  addresses dynamically, uses V2 ACK/identity verification, retries
  `--connect`, or dials WSS with a custom TLS client configuration.
- Any future HTTP control-plane bridge.

## Minimum infrastructure for the first external Mac test

1. One small dedicated public relay VM or host, separate from Oracle.
2. A domain pointed at it and a publicly trusted TLS certificate for
   WSS on TCP 443.
3. `dain-relay` running under supervision with:
   - the Mac node's Peer ID and the test peer/bridge Peer ID in
     `--allow`;
   - its identity data directory on persistent storage.
4. The Mac node binary built from this branch, run first in the safe
   resource mode already used for Phase A (`--cpu 0 --ram 0
   --storage 0`) with `--relay <WSS circuit-capable relay address>`.
5. A test service peer for ResourceInfo. Production Oracle integration
   and HTTP registration come later, through the design document and
   a separate approval.

## Preserving Oracle while integrating later

- Add the relay as a separate host/process; do not alter the Oracle
  firewall, ports, `dain-svc`, `dain-node`, or `llama-server` for the
  relay test.
- Nodes opt in individually with `--relay`; a node without the flag
  keeps its historical behavior.
- Test external registration against a disposable service/bridge
  first. Any later production service change (bridge trust,
  provenance, direct-vs-relayed registry classification, or an
  in-service control protocol) is a separately approved deployment,
  because restarting `dain-svc` also resets its in-memory credit
  ledger.
- A future Oracle node upgrade, if approved, should be node-only and
  scheduled separately from any service work; it is not a side effect
  of starting a relay.

## Reproduction

```bash
# p2p package, including the process-level smoke test
go test ./node/internal/p2p/ -count=1 -v

# advertisement test only (public-form loopback alias required)
sudo ip addr add 45.33.10.20/32 dev lo
go test ./node/internal/p2p/ -run TestAutoRelayAdvertisesCircuitAddr -count=1 -v
```

The relay and harness binaries are `node/cmd/relay` and
`node/cmd/relay-smoke`.
