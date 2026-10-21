// Command admission-probe collects ONE real single-node measurement
// from a running llama-server and emits it as a placement.Measurement
// JSON document.
//
// It performs a real chat completion against the given engine URL,
// records the wall-clock time itself, and takes prefill/decode timings
// from the engine's own "timings" report. Anything the engine does not
// report stays Measured=false in the output — the probe never invents
// numbers. It measures the single-node role only; coordinator/worker
// (joint) measurements require a joint run, which this tool does not
// perform or claim.
//
// Usage:
//
//	admission-probe -engine-url http://127.0.0.1:18099 \
//	  -model-id qwen2.5-0.5b-q4 -weights models/qwen2.5-0.5b-instruct-q4_k_m.gguf \
//	  -node-id local-vm -prompt "Explain gravity in one sentence." -out measurement.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"distributed-ai/service/internal/placement"
)

type chatResponse struct {
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Timings *struct {
		PromptMs           float64 `json:"prompt_ms"`
		PredictedMs        float64 `json:"predicted_ms"`
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func main() {
	engineURL := flag.String("engine-url", "http://127.0.0.1:18099", "llama-server base URL")
	modelID := flag.String("model-id", "", "model ID label for the measurement")
	weights := flag.String("weights", "", "weights file to SHA-256 (ties the measurement to exact weights)")
	nodeID := flag.String("node-id", "local", "node ID label")
	prompt := flag.String("prompt", "Explain gravity in one sentence.", "prompt to run")
	maxTokens := flag.Int("max-tokens", 64, "max tokens to generate")
	out := flag.String("out", "", "also write the measurement JSON to this file")
	flag.Parse()

	m := placement.Measurement{
		ModelID: *modelID,
		NodeID:  *nodeID,
		Role:    placement.RoleSingle,
		Source:  "llama-server /v1/chat/completions (wall clock + engine timings)",
		At:      time.Now(),
	}
	if *weights != "" {
		h, err := placement.HashWeightsFile(*weights)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hash weights:", err)
			os.Exit(1)
		}
		m.ModelWeightsHash = h
	}

	body, _ := json.Marshal(map[string]any{
		"model":      *modelID,
		"messages":   []map[string]string{{"role": "user", "content": *prompt}},
		"max_tokens": *maxTokens,
	})
	client := &http.Client{Timeout: 10 * time.Minute}
	start := time.Now()
	resp, err := client.Post(*engineURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	wall := time.Since(start)
	if err != nil {
		m.Success = false
		m.ErrorType = placement.ErrTypeEngine
		fmt.Fprintln(os.Stderr, "request:", err)
		emit(m, *out)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil || resp.StatusCode != 200 || cr.Error != nil {
		m.Success = false
		m.ErrorType = placement.ErrTypeEngine
		fmt.Fprintf(os.Stderr, "engine response: status %d err %v decode %v\n", resp.StatusCode, cr.Error, err)
		emit(m, *out)
		os.Exit(1)
	}
	m.Success = true
	m.WallMs = placement.MeasureQS(float64(wall.Microseconds())/1000.0, "probe wall clock")
	m.PromptTokens = cr.Usage.PromptTokens
	m.GenTokens = cr.Usage.CompletionTokens
	if cr.Timings != nil {
		m.PrefillMs = placement.MeasureQS(cr.Timings.PromptMs, "llama-server timings")
		m.DecodeMs = placement.MeasureQS(cr.Timings.PredictedMs, "llama-server timings")
		if cr.Timings.PredictedPerSecond > 0 {
			m.TokensPerSecond = placement.MeasureQS(cr.Timings.PredictedPerSecond, "llama-server timings")
		}
	}
	emit(m, *out)
}

func emit(m placement.Measurement, out string) {
	data, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(data))
	if out != "" {
		if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write out:", err)
			os.Exit(1)
		}
	}
}
