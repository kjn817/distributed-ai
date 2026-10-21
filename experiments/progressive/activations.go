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

// Activation kinds.
const (
	// KindExactHidden: vectors are the model's final hidden states
	// (post final norm — the exact input of the lm_head/output.weight
	// affine map), one per single-token prompt, so no pooling is
	// involved.
	KindExactHidden = "exact_hidden_single_token"
	// KindPooled: vectors are pooled embeddings over multi-token
	// prompts. These are NOT per-position lm_head inputs and must not
	// be silently substituted for them.
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
}

// Validate checks shape consistency.
func (s *ActivationSet) Validate() error {
	if s.Kind == "" || s.Source == "" {
		return fmt.Errorf("progressive: activation set must declare kind and source")
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

// IsReal reports whether the vectors are real model activations of any
// kind (exact or pooled), as opposed to synthetic.
func (s *ActivationSet) IsReal() bool {
	return s.Kind == KindExactHidden || s.Kind == KindPooled
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
