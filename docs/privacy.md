# Privacy — Distributed AI Network (Alpha)

## What the node collects and sends

The node reports to the central service (HTTP) and to peers (libp2p):

| Data | Destination | Purpose |
|---|---|---|
| OS, CPU model/cores, RAM total, GPU model, disk total/free | Service | resource aggregation for `/network/status`, quest, placement |
| Shared % config (your `--cpu/--ram/--storage` flags) | Service | computes shared amounts; enforcement is local |
| libp2p listen addresses + peer ID | Service | dial-back verification, job dispatch, chunk transfer |
| Model IDs held | Service, peers | holder lists, replication planning |
| Chunk bytes served (counter) | Service | transfer credit metering |
| HTTP source IP | Service (implicit) | dial-back candidate |

## What the node does NOT send

- File contents outside the model chunks you chose to share.
- Anything from your disk besides the model files.
- Your identity beyond the random peer ID in `~/.distributed-ai-node/identity.key`.

## What the service stores

In-memory only (alpha): node registry (90s heartbeat expiry), credit
ledger, request metrics. Nothing is written to disk. Restarting the
service wipes it.

## Your control

- Share percentages are yours: `--cpu`, `--ram`, `--storage` (and GPU).
- The node only shares what the percentages allow; chunk serving only
  serves model files, never arbitrary disk reads.
- Delete `~/.distributed-ai-node/` to rotate your peer ID and wipe local state.
