// Command hidden-collector collects REAL hidden activations from a
// running llama-server for the certified-progressive experiment.
//
// Method (no llama.cpp modification): the server's OpenAI-compatible
// /v1/embeddings endpoint returns the model's pooled final hidden
// state. For a prompt of exactly ONE token, pooling is the identity, so
// the returned vector IS the final hidden state of that position —
// i.e. the exact input that output.weight (the lm_head affine map)
// consumes. The collector verifies the token count from the response
// usage: prompts that tokenize to more than one token are REFUSED by
// default (they would be pooled, a different object), and can only be
// included with -allow-pooled, which labels the dataset accordingly.
//
// The chat template is NOT applied (raw embeddings of the literal
// text), and BOS handling is the server's; the dataset records the
// endpoint and flags used so the run is reproducible.
//
// Usage:
//
//	hidden-collector -engine-url http://127.0.0.1:18100 \
//	  -prompts "The,Hello,Water,Fire,123,king,queen,Paris" \
//	  -model qwen2.5-0.5b-instruct -out hidden.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"distributed-ai/experiments/progressive"
)

type embResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

func fetchEmbedding(client *http.Client, url, text string) ([]float64, int, error) {
	body, _ := json.Marshal(map[string]any{"input": text, "model": "default"})
	resp, err := client.Post(url+"/v1/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("embeddings endpoint: HTTP %d", resp.StatusCode)
	}
	var er embResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, 0, err
	}
	if len(er.Data) != 1 || len(er.Data[0].Embedding) == 0 {
		return nil, 0, fmt.Errorf("unexpected embeddings payload shape")
	}
	return er.Data[0].Embedding, er.Usage.PromptTokens, nil
}

func main() {
	engineURL := flag.String("engine-url", "http://127.0.0.1:18100", "llama-server base URL (must run with --embedding)")
	promptsCSV := flag.String("prompts", "", "comma-separated prompt texts")
	promptsFile := flag.String("prompts-file", "", "file with one prompt per line (overrides -prompts)")
	model := flag.String("model", "", "model label recorded in the dataset")
	out := flag.String("out", "", "output dataset JSON path (required)")
	allowPooled := flag.Bool("allow-pooled", false, "accept multi-token prompts (dataset is then labeled pooled, NOT exact hidden states)")
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
		Kind:   progressive.KindExactHidden,
		Source: fmt.Sprintf("llama-server /v1/embeddings at %s (server started with --embedding); single-token prompts only, pooling is identity", *engineURL),
		Model:  *model,
	}
	pooled := 0
	skipped := 0
	for _, p := range prompts {
		vec, ntok, err := fetchEmbedding(client, *engineURL, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "prompt %q: %v\n", p, err)
			os.Exit(1)
		}
		if set.HiddenSize == 0 {
			set.HiddenSize = len(vec)
		}
		if len(vec) != set.HiddenSize {
			fmt.Fprintf(os.Stderr, "prompt %q: width %d != %d\n", p, len(vec), set.HiddenSize)
			os.Exit(1)
		}
		if ntok != 1 {
			if !*allowPooled {
				fmt.Fprintf(os.Stderr, "prompt %q tokenized to %d tokens — SKIPPED (would be pooled, not an exact hidden state); pass -allow-pooled to include pooled vectors\n", p, ntok)
				skipped++
				continue
			}
			pooled++
		}
		set.Vectors = append(set.Vectors, vec)
		fmt.Fprintf(os.Stderr, "collected %q (%d token(s), width %d)\n", p, ntok, len(vec))
	}
	if pooled > 0 {
		set.Kind = progressive.KindPooled
		set.Source += "; WARNING: dataset contains pooled multi-token vectors"
	}
	if len(set.Vectors) == 0 {
		fmt.Fprintln(os.Stderr, "no vectors collected (all prompts skipped or failed)")
		os.Exit(1)
	}
	if err := progressive.SaveActivations(*out, set); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %d vectors (kind=%s, skipped %d multi-token) to %s\n", len(set.Vectors), set.Kind, skipped, *out)
}
