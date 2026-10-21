package progressive

// Activation datasets: real hidden states vs synthetic vectors, kept
// strictly apart by the Kind field. A dataset that does not declare
// itself real (and how it was obtained) is treated as synthetic by the
// experiment runner's labels.

import (
	"encoding/json"
	"fmt"
	"os"
)

// Activation kinds. The exact_hidden_* kinds may ONLY be assigned by
// the logit verification step (cmd/hidden-verify) after reconstructed
// logits from these vectors match the server's real logits. Collection
// alone produces candidates — an earlier revision labeled
// L2-normalized embeddings as exact hidden states; that was wrong
// (llama.cpp normalizes embeddings by default, embd_normalize=2) and
// is corrected here and in the docs.
const (
	// KindHiddenCandidate: raw vectors believed to be hidden states
	// (unnormalized, per-token, pooling none) but NOT yet verified
	// against real logits.
	KindHiddenCandidate = "hidden_candidate_unverified"
	// KindEmbeddingNormalized: L2-normalized embedding vectors. These
	// are NOT hidden states (direction only, magnitude destroyed).
	KindEmbeddingNormalized = "embedding_l2_normalized"
	// KindExactHiddenSingle: verified final hidden states, one per
	// single-token prompt.
	KindExactHiddenSingle = "exact_hidden_single_token"
	// KindExactHiddenContextual: verified final hidden states of the
	// LAST token of a multi-token context.
	KindExactHiddenContextual = "exact_hidden_contextual_last"
	// KindPooled: pooled embeddings over multi-token prompts. NOT
	// per-position lm_head inputs.
	KindPooled = "pooled_multi_token"
	// KindSynthetic: generated vectors (seeded Gaussian).
	KindSynthetic = "synthetic"
)

// ActivationSet is a labeled collection of activation vectors.
type ActivationSet struct {
	Kind       string      `json:"kind"`
	Source     string      `json:"source"` // how the vectors were obtained
	Model      string      `json:"model,omitempty"`
	HiddenSize int         `json:"hidden_size"`
	Vectors    [][]float64 `json:"vectors"`
	// Prompts[i] is the text that produced Vectors[i].
	Prompts []string `json:"prompts,omitempty"`
	// Positions[i]: "single" (only token) or "last" (last token of a
	// multi-token context).
	Positions []string `json:"positions,omitempty"`
	// Norms[i] is the L2 norm of Vectors[i] as collected.
	Norms []float64 `json:"norms,omitempty"`
	// Normalized: the collecting path L2-normalized the vectors
	// (norms ≈ 1). Normalized vectors are not hidden states.
	Normalized bool `json:"normalized"`
	// VerifiedLogits: cmd/hidden-verify confirmed that logits
	// reconstructed from these vectors (× output.weight, no bias —
	// the model has none) match the server's real next-token logits.
	VerifiedLogits bool   `json:"verified_logits"`
	Verification   string `json:"verification,omitempty"`
}

// Validate checks shape consistency and label discipline: an
// exact-hidden kind without a recorded logit verification is a
// labeling error and is rejected.
func (s *ActivationSet) Validate() error {
	if s.Kind == "" || s.Source == "" {
		return fmt.Errorf("progressive: activation set must declare kind and source")
	}
	if (s.Kind == KindExactHiddenSingle || s.Kind == KindExactHiddenContextual) && !s.VerifiedLogits {
		return fmt.Errorf("progressive: kind %q requires a passed logit verification (verified_logits)", s.Kind)
	}
	if len(s.Vectors) == 0 {
		return fmt.Errorf("progressive: activation set is empty")
	}
	for i, v := range s.Vectors {
		if len(v) != s.HiddenSize {
			return fmt.Errorf("progressive: vector %d has width %d, want %d", i, len(v), s.HiddenSize)
		}
	}
	return nil
}

// IsReal reports whether the vectors came from a real model (any
// kind), as opposed to synthetic vectors. Real does NOT imply verified
// hidden states — see VerifiedLogits.
func (s *ActivationSet) IsReal() bool {
	switch s.Kind {
	case KindHiddenCandidate, KindEmbeddingNormalized,
		KindExactHiddenSingle, KindExactHiddenContextual, KindPooled:
		return true
	}
	return false
}

// IsVerifiedHidden reports whether the set carries the exact-hidden
// label backed by a passed logit verification.
func (s *ActivationSet) IsVerifiedHidden() bool {
	return s.VerifiedLogits &&
		(s.Kind == KindExactHiddenSingle || s.Kind == KindExactHiddenContextual)
}

// SaveActivations writes a dataset as JSON.
func SaveActivations(path string, s *ActivationSet) error {
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// LoadActivations reads and validates a dataset JSON.
func LoadActivations(path string) (*ActivationSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s ActivationSet
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("progressive: decode activation set: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}
