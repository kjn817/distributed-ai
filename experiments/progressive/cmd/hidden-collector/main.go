// Command hidden-collector collects candidate hidden activations
// from a running llama-server for the certified-progressive experiment.
//
// Method (no llama.cpp modification): start a DEDICATED embedding
// instance with
//
//	llama-server --embedding --pooling none --embd-normalize -1
//
// and use the native POST /embedding endpoint, which returns one
// vector per token position (the OpenAI /v1/embeddings endpoint
// rejects pooling=none). With normalization disabled these vectors are
// the raw per-position outputs of the final norm — candidates for the
// exact input of output.weight.
//
// Candidates are NOT hidden states until proven: this tool labels the
// dataset hidden_candidate_unverified (or embedding_l2_normalized if
// the norms come back ≈1, meaning the server normalized anyway).
// cmd/hidden-verify performs the logit comparison that can promote a
// dataset to an exact_hidden_* kind.
//
// For multi-token prompts the collector keeps the LAST position's
// vector (position "last") — the hidden state that predicts the next
// token in that context. Single-token prompts are position "single".
//
// Usage:
//
//	hidden-collector -engine-url http://127.0.0.1:18100 \
//	  -prompts "Hello,The capital of France is" \
//	  -model qwen2.5-0.5b-instruct -out hidden-candidates.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"distributed-ai/experiments/progressive"
)

// native /embedding response: [{"index":0,"embedding":[[...], ...]}]
// with one inner vector per token position.
type embResponse struct {
	Index     int         `json:"index"`
	Embedding [][]float64 `json:"embedding"`
}

func fetchTokens(client *http.Client, url, text string) ([][]float64, error) {
	body, _ := json.Marshal(map[string]any{"content": text})
	resp, err := client.Post(url+"/embedding", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embedding endpoint: HTTP %d (server must run --embedding --pooling none)", resp.StatusCode)
	}
	var arr []embResponse
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, err
	}
	if len(arr) != 1 || len(arr[0].Embedding) == 0 {
		return nil, fmt.Errorf("unexpected /embedding payload shape")
	}
	return arr[0].Embedding, nil
}

func l2(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func main() {
	engineURL := flag.String("engine-url", "http://127.0.0.1:18100", "llama-server base URL (must run --embedding --pooling none --embd-normalize -1)")
	promptsCSV := flag.String("prompts", "", "comma-separated prompt texts")
	promptsFile := flag.String("prompts-file", "", "file with one prompt per line (overrides -prompts)")
	model := flag.String("model", "", "model label recorded in the dataset")
	out := flag.String("out", "", "output dataset JSON path (required)")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	var prompts []string
	if *promptsFile != "" {
		data, err := os.ReadFile(*promptsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if s := strings.TrimSpace(line); s != "" {
				prompts = append(prompts, s)
			}
		}
	} else if *promptsCSV != "" {
		prompts = strings.Split(*promptsCSV, ",")
	}
	if len(prompts) == 0 {
		fmt.Fprintln(os.Stderr, "no prompts given")
		os.Exit(2)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	set := &progressive.ActivationSet{
		Kind:   progressive.KindHiddenCandidate,
		Source: fmt.Sprintf("llama-server native /embedding at %s (dedicated instance: --embedding --pooling none --embd-normalize -1); per-token vectors, last position kept for multi-token prompts; NOT yet logit-verified", *engineURL),
		Model:  *model,
	}
	allUnit := true
	for _, p := range prompts {
		vecs, err := fetchTokens(client, *engineURL, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "prompt %q: %v\n", p, err)
			os.Exit(1)
		}
		v := vecs[len(vecs)-1]
		if set.HiddenSize == 0 {
			set.HiddenSize = len(v)
		}
		if len(v) != set.HiddenSize {
			fmt.Fprintf(os.Stderr, "prompt %q: width %d != %d\n", p, len(v), set.HiddenSize)
			os.Exit(1)
		}
		pos := "single"
		if len(vecs) > 1 {
			pos = "last"
		}
		n := l2(v)
		if math.Abs(n-1) > 0.01 {
			allUnit = false
		}
		set.Vectors = append(set.Vectors, v)
		set.Prompts = append(set.Prompts, p)
		set.Positions = append(set.Positions, pos)
		set.Norms = append(set.Norms, n)
		fmt.Fprintf(os.Stderr, "collected %q (%d token(s), position=%s, L2 norm %.2f)\n", p, len(vecs), pos, n)
	}
	if allUnit {
		// The server normalized despite the flags — these are
		// direction-only embeddings, not hidden states.
		set.Kind = progressive.KindEmbeddingNormalized
		set.Normalized = true
		set.Source += "; WARNING: all norms ≈ 1 — the server L2-normalized the output"
	}
	if err := progressive.SaveActivations(*out, set); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %d vectors (kind=%s) to %s — run hidden-verify before calling them hidden states\n", len(set.Vectors), set.Kind, *out)
}
