package main

import "testing"

func controlledPair() (prior, current map[string]any) {
	base := func() map[string]any {
		return map[string]any{
			"prompt_sha256":        promptHash("What is the capital of France?"),
			"max_tokens_requested": float64(32),
			"cache_prompt":         false,
			"gen_tokens":           float64(9),
		}
	}
	return base(), base()
}

func TestControlledComparisonAcceptsOnlyControlledPairs(t *testing.T) {
	prior, current := controlledPair()
	if ok, why := controlledComparison(prior, current); !ok {
		t.Fatalf("controlled pair rejected: %s", why)
	}

	// Different prompt.
	prior, current = controlledPair()
	current["prompt_sha256"] = promptHash("A different prompt")
	if ok, _ := controlledComparison(prior, current); ok {
		t.Error("different prompt must invalidate the comparison")
	}

	// Different token cap.
	prior, current = controlledPair()
	current["max_tokens_requested"] = float64(16)
	if ok, _ := controlledComparison(prior, current); ok {
		t.Error("different token cap must invalidate the comparison")
	}

	// Cache not disabled (or not recorded) in the prior run: this is
	// the pre-correction summary shape, and it must stay incomparable
	// rather than quietly producing an error percentage.
	prior, current = controlledPair()
	delete(prior, "cache_prompt")
	if ok, _ := controlledComparison(prior, current); ok {
		t.Error("unrecorded cache condition must invalidate the comparison")
	}

	// Same cap but different actual generated-token counts.
	prior, current = controlledPair()
	current["gen_tokens"] = float64(12)
	if ok, _ := controlledComparison(prior, current); ok {
		t.Error("different generated-token counts must invalidate the comparison")
	}

	// No prompt hash at all (legacy summary).
	prior, current = controlledPair()
	delete(prior, "prompt_sha256")
	if ok, _ := controlledComparison(prior, current); ok {
		t.Error("missing prompt hash must invalidate the comparison")
	}
}

func TestPromptHashIsStableAndContentBound(t *testing.T) {
	if promptHash("abc") != promptHash("abc") {
		t.Error("prompt hash must be deterministic")
	}
	if promptHash("abc") == promptHash("abd") {
		t.Error("different prompts must hash differently")
	}
	if len(promptHash("abc")) != 64 {
		t.Error("prompt hash must be a SHA-256 hex digest")
	}
}
