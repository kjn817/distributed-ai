// Package credit implements the network's internal test credits.
//
// Policy (v1, per the dev plan §21–22):
//   - No cash in, no cash out. Credits are earned by contributing and spent
//     on AI. They have no monetary value.
//   - Earn: valid compute/storage/network provision.
//   - inference: CreditsPerToken per token served (prompt + completion)
//   - chunk transfer: CreditsPerGBXfer per GB served over P2P
//   - storage task: CreditsPerGBStored per GB when a node becomes a new
//     holder (successful task completion, not mere uptime)
//   - The founder takes FounderSharePct of every earning (network
//     maintenance share, transparent).
//   - Spend: AI use deducts actual tokens served.
//
// "Don't pay just for being online" (§22): heartbeats alone earn nothing.
package credit

// v1 rates. Internal test credits; tuned later against real data.
const (
	CreditsPerToken    = 1  // per token served
	CreditsPerGBXfer   = 1  // per GB of chunk bytes served
	CreditsPerGBStored = 10 // per GB when becoming a new holder
	FounderSharePct    = 1  // founder's cut of every earning
	GB                 = 1024 * 1024 * 1024
)
