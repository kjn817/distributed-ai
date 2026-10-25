# Phase A — private HTTP registration for external nodes

Status: **design only.** This document does not deploy a bridge, change
`dain-svc`, open a port, or make the Oracle HTTP API reachable from the
internet.

## Problem

A node process has two different control relationships:

1. **Peer connectivity / ResourceInfo:** libp2p direct or Circuit Relay
   v2 connection to another peer. ResourceInfo V2 proves delivery with
   an ACK and binds the payload to the authenticated `RemotePeer`.
2. **Service registration / heartbeat:** HTTP calls to the DAIN service
   (`POST /v1/nodes/register`, `POST /v1/nodes/heartbeat`). These update
   the service registry, model-holder state, replication assignments
   and some credit-related reports.

The first does **not** replace the second. A Mac node can exchange
ResourceInfo through a relay while the service still knows nothing
about it. Conversely, exposing the existing loopback HTTP API through
a public tunnel would create a new public API surface and is not an
acceptable shortcut.

Relevant current facts, checked against the code:

- `node/internal/reporter` sends registration and heartbeat over plain
  HTTP to the configured service URL.
- The service's registration handler currently accepts a JSON node
  payload without a node credential. It also derives a dial-back
  candidate from the HTTP source address. Neither property is safe to
  expose by simply forwarding internet HTTP requests.
- `service/internal/p2pnet` already gives the service a persistent
  libp2p host for dial-back and job dispatch, but it has no HTTP-over-
  libp2p registration handler and no relay-client configuration.
- `protocol.ConnectPeer` now treats a `/p2p-circuit` address as a
  complete dial target. A successful circuit connection must not be
  recorded as direct reachability merely because a connection exists.

## Security requirements for either design

1. Only two routes are proxied:
   - `POST /v1/nodes/register`
   - `POST /v1/nodes/heartbeat`
   No generic HTTP proxying, no chat/completions route, no credit
   spending route, and no account/API-key claim route.
2. The node identity is the libp2p stream's authenticated
   `RemotePeer`. A request is rejected before forwarding when the
   payload node/peer ID differs from `RemotePeer`. A relay allowlist is
   an admission control; it is not a substitute for this binding.
3. Requests are bounded: canonical JSON, a small body limit (64 KiB is
   ample for the current payloads), a short deadline, method/path
   allowlisting, and per-peer rate limits.
4. The bridge/proxy must not pass through arbitrary identity headers.
   It derives identity itself and strips client-supplied forwarding
   headers.
5. The service must not treat the bridge's loopback address as the
   node's source IP. Registration through this path has no HTTP source
   IP candidate; dial-back uses the reported libp2p addresses.
6. Reachability must be classified by the connection actually used:
   - direct connection succeeds → `DirectOK` may be set;
   - only a `/p2p-circuit` connection succeeds → relayed reachability,
     **not** `DirectOK`.
   A production registry change should carry that distinction
   explicitly instead of overloading the current boolean.
7. Replay must not create duplicate side effects. Registration is
   mostly idempotent, but heartbeat transfer reports feed credit
   accounting. The production protocol needs a per-peer sequence or
   idempotency key and a service/bridge dedupe window; until that
   exists, a test bridge must not treat a replayed heartbeat as a new
   transfer report.
8. Node identity keys and credit API keys remain separate credentials.
   This channel never carries a credit API key.

## Option A — authenticated libp2p control protocol inside `dain-svc`

Add a versioned stream protocol, for example
`/distributed-ai/control/0.1.0`, to the service's existing `p2pnet`
host. A request envelope contains only a request ID, one of the two
allowed operations, and the canonical JSON body. The service handler:

- reads `RemotePeer` from the stream;
- validates it against the payload identity;
- invokes the same internal registration/heartbeat logic used by the
  HTTP handlers, with an explicit `via=libp2p` provenance;
- returns the HTTP-like status and JSON response over the same stream.

The service host must itself be reachable by external nodes: either it
reserves through the same DAIN relay or it has a separately approved
public libp2p listener. The node reaches it at a direct address or at
`…/p2p-circuit/p2p/<servicePeerID>`; relayed streams use libp2p's
transient-stream opt-in.

**Advantages**

- Smallest trusted surface: no separate registrar process and no
  loopback impersonation problem.
- Identity, provenance and reachability classification can be handled
  in the service itself.
- No public HTTP listener is added.

**Costs / blockers**

- Requires changing and eventually restarting/redeploying `dain-svc`,
  so it is outside this Phase A prototype round.
- The current in-memory registry and credit ledger make any future
  service restart an operational event, not a routine deployment.
- The service host's relay configuration and recovery behavior become
  production responsibilities of `dain-svc`.

## Option B — co-located outbound Control Plane bridge (recommended next prototype)

Run a separate `dain-control-bridge` process beside the service. It
has its own persistent libp2p identity and makes only outbound
connections to the DAIN relay, reserving a relay slot like any other
peer. It does not need an inbound public port.

Flow:

```text
external node
  │  libp2p stream, direct or via Circuit Relay v2
  │  protocol /distributed-ai/control/0.1.0
  ▼
dain-control-bridge  (same host as the service)
  │  validates RemotePeer == payload node ID
  │  route/body/rate/idempotency checks
  ▼
127.0.0.1 service HTTP API (register / heartbeat only)
```

The node opens a stream to the bridge at its circuit address. The
bridge forwards the bounded request to the loopback service and
returns the response on the same stream, so heartbeat replication
assignments still reach the node. If the relay or bridge restarts, the
node retries registration/heartbeat with backoff; the bridge re-reserves
when its relay connection returns.

For a **local prototype**, the bridge can forward to the existing
loopback endpoints without changing `dain-svc`, provided it enforces
the identity binding itself. That is not yet the production end state:

- the current HTTP handler would see the bridge as the HTTP source and
  record a useless/incorrect source-IP candidate;
- the service cannot distinguish bridge-authenticated requests from
  any other loopback caller;
- heartbeat idempotency is not implemented by the current handler.

Before production use, add one small service-side change in an
approved deployment window: accept bridge requests only with a
bridge-only internal credential, take the authenticated peer and
`via` provenance from the bridge channel, suppress HTTP source-IP
dial-back for those requests, and honor an idempotency key. The bridge
private key and internal credential stay on the service host and are
never distributed to nodes.

**Advantages**

- No `dain-svc` restart is needed to prototype the protocol locally.
- The production service remains loopback-only; both the node and the
  bridge initiate network connections outward to the relay.
- The bridge can be stopped independently without stopping the API,
  node or inference processes.

**Costs / risks**

- The bridge is trusted infrastructure: compromise of the bridge host
  is compromise of the registration path. It needs the same host
  hardening and key protection as the service.
- It is another process to supervise, monitor and upgrade.
- Without the service-side provenance/idempotency change above, it is
  a test mechanism, not a production registration path.

## Comparison

| Criterion | A: in-service protocol | B: co-located bridge |
|---|---|---|
| Public HTTP exposure | none | none |
| New inbound public port | none if relayed | none |
| `dain-svc` change/restart | required | not for a local prototype; small change required for production |
| Trusted components | service only | service + bridge |
| Identity binding | service sees `RemotePeer` directly | bridge enforces, service must trust bridge channel |
| Source-IP/provenance handling | native | needs explicit service-side support |
| Independent failure/recovery | tied to service | bridge can restart independently |
| Best use | production end state | next isolated prototype and migration path |

## Recommendation

Build **B first as an isolated prototype**, using the same versioned
control protocol that A would eventually absorb. Keep the protocol
implementation independent of the bridge process so it can move into
`dain-svc` without changing node behavior. Do not deploy B to Oracle in
this round.

Minimal future implementation set:

- `node/internal/controlplane/` — request/response envelope, route
  allowlist, identity validation helpers and tests.
- `node/cmd/control-bridge/` — persistent identity, relay client,
  bounded forwarding to a loopback service URL, reconnect/backoff.
- `node/internal/reporter/` — an optional control-plane transport that
  produces the same register/heartbeat payloads as HTTP.
- Service-side production follow-up (separate approval): bridge
  credential, authenticated-peer provenance, no source-IP candidate
  for bridged requests, idempotency, and direct-vs-relayed registry
  classification.

## Acceptance tests for the future prototype

1. Allowlisted node registers through a relay; the registry entry's ID
   equals the stream `RemotePeer`.
2. A payload claiming another peer ID is rejected before forwarding.
3. Any route other than register/heartbeat is rejected.
4. A rogue peer cannot open the control protocol through the relay
   allowlist; a removed peer loses access at reservation renewal.
5. Heartbeat responses deliver replication assignments unchanged.
6. Replaying the same heartbeat idempotency key has no duplicate
   credit side effect.
7. Relay restart: bridge and node recover without process restarts;
   registration is retried, not duplicated unsafely.
8. A circuit-only node is recorded as relay-reachable, never
   `DirectOK`; a later direct/DCUtR success can upgrade the
   classification.
9. The service host records no bridge loopback address as the node's
   source IP.
10. No HTTP listener becomes reachable from a non-loopback interface
    as part of the test.

## Minimum infrastructure for an external Mac test

- One dedicated relay host reachable from the Mac network, with a
  stable public address or domain.
- For WSS: TCP 443 with a publicly trusted certificate for that domain
  (self-signed is local-test only). Plain TCP/QUIC relay listeners are
  optional fallbacks, not substitutes in a 443-only network.
- The relay allowlist contains the Mac node's Peer ID and the bridge
  or service Peer ID.
- The bridge runs beside a **test** service instance first. The
  production Oracle service is integrated only after the acceptance
  tests and a separate deployment approval.

## Preserving the running Oracle services

The design keeps `dain-svc`, `dain-node` and `llama-server` untouched:

- the relay is a separate host/process;
- the bridge is a separate process with its own identity and data
  directory;
- no existing port, firewall rule, Worker setting or credit-ledger
  code changes;
- integration with the production service is a later, explicit step,
  not a side effect of relay deployment.
