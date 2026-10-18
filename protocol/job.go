// Package protocol holds the wire types shared by the service and the node
// runtime for job assignment. Kept at the top level so both can import it
// without an import cycle.
//
// Job protocol types: the wire format for assigning inference work to nodes.
// Protocol ID and JSON field names are frozen for wire compatibility.
// v1: FULL_MATERIALIZE only — the assigned node assembles the full model
// file before executing (see docs/manifest-spec.md execution strategies).
package protocol

import "github.com/libp2p/go-libp2p/core/protocol"

// JobProtocol is the stream protocol for inference job assignment.
const JobProtocol = protocol.ID("/distributed-ai/job/1")

// JobRequest assigns one inference job to a worker node.
type JobRequest struct {
	JobID     string            `json:"job_id"`
	ModelID   string            `json:"model_id"`
	Strategy  string            `json:"strategy"` // "FULL_MATERIALIZE" or "DISTRIBUTED"
	Messages  []ChatMessage     `json:"messages"`
	MaxTokens int               `json:"max_tokens"`
	Params    map[string]string `json:"params,omitempty"`
	// RpcBackends lists "host:port" of rpc-server workers for joint compute.
	// When non-empty, the executor starts llama-server with --rpc backends
	// (one request computed jointly by coordinator + workers).
	RpcBackends []string `json:"rpc_backends,omitempty"`
}

// ChatMessage is one OpenAI-style chat message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// JobResult is the worker's completed inference result.
type JobResult struct {
	JobID        string `json:"job_id"`
	Content      string `json:"content"`
	PromptTokens int    `json:"prompt_tokens"`
	GenTokens    int    `json:"gen_tokens"`
	DurationMs   int64  `json:"duration_ms"`
	Error        string `json:"error,omitempty"`
}
