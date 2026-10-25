// Command rpc-probe measures a REAL joint inference run over ggml-rpc
// on the local loopback and feeds the measurements into Adaptive
// Compute Admission (shadow). It runs in an isolated local test
// environment only: both processes are local, the path is loopback,
// and nothing here is evidence about WAN (Mac↔Oracle) performance —
// the NetworkPath label says so in every record.
//
// What it measures, and how:
//   - wall_ms: probe wall clock around one chat completion that the
//     coordinator executes with ALL layers on the RPC worker (-ngl 99).
//     That is full offload to ONE worker — not split-layer CPU+GPU
//     joint computation, and this probe never reports it as such.
//   - process_cpu_ms per role: process CPU time (utime+stime from
//     /proc/<pid>/stat) consumed by the coordinator and worker
//     processes during the request window. This is CPU consumption,
//     NOT pure compute: it can include spin and scheduler effects and
//     can exceed wall time. It is recorded in ProcessCPUMs only.
//   - compute_ms: this probe has no scope-valid pure-compute source
//     per role, so it leaves ComputeMs unmeasured. Admission therefore
//     degrades honestly instead of pricing a CPU delta as compute.
//   - rtt_ms: median TCP connect time to the RPC port (loopback
//     handshake), labeled as such.
//   - bandwidth_mbps: bulk TCP throughput over a probe-owned loopback
//     connection (32 MiB), same path family.
//   - tensor_bytes: loopback interface tx-byte delta during the
//     request window — total bytes moved (tensor payload + protocol
//     overhead + the probe's own small HTTP), labeled as such.
//   - rtt_rounds: 1 (prompt graph) + generated tokens (one ggml-rpc
//     graph compute per decode step) — protocol-known for ggml-rpc.
//   - residual_after_process_cpu_ms: a diagnostic arithmetic residual
//     (wall − coordinator CPU − worker CPU − network estimate). It is
//     NOT recorded as measured overhead: its inputs are process CPU
//     deltas, so it is not a scope-valid overhead measurement.
//
// Outputs: coordinator/worker Measurement JSON files and a summary
// JSON with the shadow decision. A prediction-vs-actual error is
// reported ONLY for a cross-run comparison whose inputs came from a
// prior run AND whose run conditions are verified to match (same
// prompt hash, same token cap, prompt cache disabled, same actual
// generated-token count). A same-run figure would be circular — the
// decision was built from that very run — so it is reported as null
// with the reason, never as 0% "prediction success".
//
// Usage:
//
//	rpc-probe -coordinator-url http://127.0.0.1:18099 \
//	  -rpc-port 19000 -coord-pid 1234 -worker-pid 1235 \
//	  -model-id qwen2.5-0.5b-q4 -weights model.gguf \
//	  -single-measurement single.json -out-prefix /tmp/rpc-run1
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"distributed-ai/service/internal/placement"
	"distributed-ai/service/internal/registry"
)

const networkPath = "local-loopback-ggml-rpc (NOT WAN evidence)"

func loTxBytes() (uint64, error) {
	b, err := os.ReadFile("/sys/class/net/lo/statistics/tx_bytes")
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func procCPUMs(pid int, hz float64) (float64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// comm may contain spaces inside parens; fields resume after ") ".
	s := string(b)
	i := strings.LastIndex(s, ") ")
	if i < 0 {
		return 0, fmt.Errorf("unparsable /proc stat")
	}
	f := strings.Fields(s[i+2:])
	// f[0] is state (field 3); utime=field14 -> f[11], stime=field15 -> f[12].
	if len(f) < 13 {
		return 0, fmt.Errorf("short /proc stat")
	}
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (ut + st) / hz * 1000, nil
}

func tcpRTTMedian(addr string, n int) float64 {
	var ds []float64
	for i := 0; i < n; i++ {
		start := time.Now()
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			continue
		}
		ds = append(ds, float64(time.Since(start).Microseconds())/1000.0)
		c.Close()
	}
	if len(ds) == 0 {
		return 0
	}
	sort.Float64s(ds)
	return ds[len(ds)/2]
}

func bulkBandwidthMbps() float64 {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1<<20)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return 0
	}
	defer c.Close()
	const total = 32 << 20
	buf := make([]byte, 1<<20)
	start := time.Now()
	sent := 0
	for sent < total {
		n, err := c.Write(buf)
		if err != nil {
			return 0
		}
		sent += n
	}
	return float64(total) * 8 / time.Since(start).Seconds() / 1e6
}

type chatResp struct {
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Timings *struct {
		PromptMs    float64 `json:"prompt_ms"`
		PredictedMs float64 `json:"predicted_ms"`
	} `json:"timings"`
}

// promptHash identifies the exact prompt text in a run summary
// without recording the prompt itself.
func promptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

func summaryString(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok && v != ""
}

func summaryFloat(m map[string]any, key string) (float64, bool) {
	v, ok := m[key].(float64)
	return v, ok
}

func summaryBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key].(bool)
	return v, ok
}

// controlledComparison decides whether a prior run's prediction may
// honestly be compared with the current run's actual time. Repeated
// experiments are comparable only when the run conditions were
// controlled: identical prompt (by hash), identical token cap,
// prompt caching disabled in BOTH runs, and the same number of
// actually generated tokens. Anything missing or different makes the
// comparison invalid — no error percentage may be reported.
func controlledComparison(prior, current map[string]any) (bool, string) {
	pHash, ok := summaryString(prior, "prompt_sha256")
	if !ok {
		return false, "prior run summary has no prompt_sha256 — its run conditions cannot be verified"
	}
	cHash, _ := summaryString(current, "prompt_sha256")
	if pHash != cHash {
		return false, "prompt differs between the prior and current runs"
	}
	pMax, okP := summaryFloat(prior, "max_tokens_requested")
	cMax, okC := summaryFloat(current, "max_tokens_requested")
	if !okP || !okC || pMax != cMax {
		return false, "max_tokens_requested differs or is unrecorded between runs"
	}
	for name, m := range map[string]map[string]any{"prior": prior, "current": current} {
		cp, ok := summaryBool(m, "cache_prompt")
		if !ok || cp {
			return false, name + " run did not record cache_prompt=false — cache conditions were not controlled"
		}
	}
	pGen, okP := summaryFloat(prior, "gen_tokens")
	cGen, okC := summaryFloat(current, "gen_tokens")
	if !okP || !okC || pGen != cGen {
		return false, "actual generated token counts differ or are unrecorded between runs"
	}
	return true, "prior-run prediction compared under controlled conditions (same prompt hash, token cap, cache_prompt=false, generated-token count)"
}

func main() {
	coordURL := flag.String("coordinator-url", "http://127.0.0.1:18099", "coordinator llama-server base URL")
	rpcPort := flag.Int("rpc-port", 19000, "ggml-rpc-server port")
	coordPID := flag.Int("coord-pid", 0, "coordinator process PID")
	workerPID := flag.Int("worker-pid", 0, "ggml-rpc-server process PID")
	modelID := flag.String("model-id", "", "model ID label")
	weights := flag.String("weights", "", "weights file (SHA-256 recorded)")
	prompt := flag.String("prompt", "What is the capital of France?", "prompt for the joint run")
	maxTokens := flag.Int("max-tokens", 32, "max tokens")
	singlePath := flag.String("single-measurement", "", "admission-probe single-node measurement JSON (baseline)")
	predictFrom := flag.String("predict-from", "", "prefix of a PRIOR probe run: build the admission decision from ITS coordinator/worker measurements, and compare its predicted joint time against THIS run's measured wall (a same-run comparison would be circular — the residual definition makes it identically zero)")
	outPrefix := flag.String("out-prefix", "", "output prefix: writes <prefix>-coordinator.json, <prefix>-worker.json, <prefix>-summary.json")
	userHZ := flag.Float64("user-hz", 100, "clock ticks per second for /proc stat")
	flag.Parse()
	if *coordPID == 0 || *workerPID == 0 || *outPrefix == "" {
		fmt.Fprintln(os.Stderr, "-coord-pid, -worker-pid and -out-prefix are required")
		os.Exit(2)
	}
	hash := ""
	if *weights != "" {
		h, err := placement.HashWeightsFile(*weights)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hash:", err)
			os.Exit(1)
		}
		hash = h
	}

	rtt := tcpRTTMedian(fmt.Sprintf("127.0.0.1:%d", *rpcPort), 15)
	bw := bulkBandwidthMbps()

	// Request window: CPU + loopback bytes + wall.
	coordCPU0, err := procCPUMs(*coordPID, *userHZ)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coord cpu:", err)
		os.Exit(1)
	}
	workerCPU0, err := procCPUMs(*workerPID, *userHZ)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker cpu:", err)
		os.Exit(1)
	}
	lo0, err := loTxBytes()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lo bytes:", err)
		os.Exit(1)
	}
	// cache_prompt=false is part of the experiment controls: a repeat
	// run must not silently reuse the coordinator's prompt cache and
	// call the difference prediction error (or accuracy).
	body, _ := json.Marshal(map[string]any{
		"model": *modelID, "messages": []map[string]string{{"role": "user", "content": *prompt}},
		"max_tokens": *maxTokens, "cache_prompt": false,
	})
	client := &http.Client{Timeout: 10 * time.Minute}
	start := time.Now()
	resp, err := client.Post(*coordURL+"/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	wall := float64(time.Since(start).Microseconds()) / 1000.0
	coordCPU1, _ := procCPUMs(*coordPID, *userHZ)
	workerCPU1, _ := procCPUMs(*workerPID, *userHZ)
	lo1, _ := loTxBytes()
	if err != nil || resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "joint run failed: err=%v status=%v\n", err, resp)
		os.Exit(1)
	}
	var cr chatResp
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(raw, &cr); err != nil {
		fmt.Fprintln(os.Stderr, "decode:", err)
		os.Exit(1)
	}

	coordCPU := coordCPU1 - coordCPU0
	workerCPU := workerCPU1 - workerCPU0
	tensorBytes := float64(lo1 - lo0)
	rounds := 1 + cr.Usage.CompletionTokens
	transferMs := 0.0
	if bw > 0 {
		transferMs = tensorBytes * 8 / (bw * 1e3)
	}
	networkMs := rtt*float64(rounds) + transferMs
	residual := wall - coordCPU - workerCPU - networkMs

	now := time.Now()
	// Process CPU time is recorded ONLY as ProcessCPUMs. ComputeMs and
	// OverheadMs stay unmeasured in both records: this probe has no
	// scope-valid pure-compute source, and the residual is arithmetic
	// over CPU deltas, not a measured overhead.
	coord := placement.Measurement{
		ModelID: *modelID, ModelWeightsHash: hash, NodeID: "local-coord", Role: placement.RoleCoordinator,
		WallMs:        placement.MeasureQS(wall, "probe wall clock"),
		ProcessCPUMs:  placement.MeasureQS(coordCPU, "coordinator process CPU time delta (/proc stat utime+stime) — CPU consumption, not pure compute"),
		NetworkPath:   networkPath,
		RTTMs:         placement.MeasureQS(rtt, "median TCP connect time to the RPC port (loopback handshake)"),
		BandwidthMbps: placement.MeasureQS(bw, "probe bulk TCP transfer on loopback (32 MiB)"),
		RTTRounds:     rounds, RPCConnected: true,
		TensorBytes:  placement.MeasureQS(tensorBytes, "loopback tx byte delta during the request window (tensor payload + protocol overhead + probe HTTP)"),
		PromptTokens: cr.Usage.PromptTokens, GenTokens: cr.Usage.CompletionTokens,
		Success: true, Source: "rpc-probe local loopback ggml-rpc run (-ngl 99: all layers on the single RPC worker — full offload, not split-layer joint compute)", At: now,
	}
	if cr.Timings != nil {
		coord.PrefillMs = placement.MeasureQS(cr.Timings.PromptMs, "llama-server timings")
		coord.DecodeMs = placement.MeasureQS(cr.Timings.PredictedMs, "llama-server timings")
	}
	worker := placement.Measurement{
		ModelID: *modelID, ModelWeightsHash: hash, NodeID: "local-worker", Role: placement.RoleWorker,
		ProcessCPUMs: placement.MeasureQS(workerCPU, "worker process CPU time delta (/proc stat utime+stime) — CPU consumption, not pure compute"),
		NetworkPath:  networkPath,
		RPCConnected: true,
		Success:      true, Source: "rpc-probe local loopback ggml-rpc run", At: now,
	}

	summary := map[string]any{
		"environment":                    "isolated local loopback — NOT WAN (Mac↔Oracle) evidence",
		"network_path":                   networkPath,
		"layer_placement":                "-ngl 99 (all model layers on the single RPC worker device)",
		"layer_placement_interpretation": "full offload to one worker; NOT evidence of split-layer CPU+GPU joint computation",
		"joint_compute_verified":         false,
		"model_id":                       *modelID,
		"model_weights_hash":             hash,
		"wall_ms":                        wall,
		"coordinator_process_cpu_ms":     coordCPU,
		"worker_process_cpu_ms":          workerCPU,
		"process_cpu_note":               "process CPU time (utime+stime) is CPU consumption, not pure compute; it is never fed to admission as compute_ms",
		"rtt_ms_median_tcp":              rtt,
		"bandwidth_mbps":                 bw,
		"tensor_bytes_total":             tensorBytes,
		"rtt_rounds":                     rounds,
		"network_ms_est":                 networkMs,
		"residual_after_process_cpu_ms":  residual,
		"residual_note":                  "diagnostic arithmetic over process-CPU deltas only; NOT a measured overhead and never an admission input",
		"overhead_ms":                    nil,
		"prompt_sha256":                  promptHash(*prompt),
		"max_tokens_requested":           *maxTokens,
		"cache_prompt":                   false,
		"prompt_tokens":                  cr.Usage.PromptTokens,
		"gen_tokens":                     cr.Usage.CompletionTokens,
	}

	// Admission connection (shadow): single baseline + these records.
	store := placement.NewMemoryMeasurementStore(64)
	if *singlePath != "" {
		data, err := os.ReadFile(*singlePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "single measurement:", err)
			os.Exit(1)
		}
		var single placement.Measurement
		if err := json.Unmarshal(data, &single); err != nil {
			fmt.Fprintln(os.Stderr, "single measurement decode:", err)
			os.Exit(1)
		}
		single.NodeID = "local-coord" // the same local machine is the single-node baseline host
		store.Record(single)
	}
	// Decision inputs: this run's measurements by default; with
	// -predict-from, the PRIOR run's records (this run then serves as
	// the independent actual for the prediction-error comparison).
	decCoord, decWorker := coord, worker
	if *predictFrom != "" {
		load := func(path string) placement.Measurement {
			data, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "predict-from:", err)
				os.Exit(1)
			}
			var m placement.Measurement
			if err := json.Unmarshal(data, &m); err != nil {
				fmt.Fprintln(os.Stderr, "predict-from decode:", err)
				os.Exit(1)
			}
			return m
		}
		decCoord = load(*predictFrom + "-coordinator.json")
		decWorker = load(*predictFrom + "-worker.json")
		// Prior probe runs (pre-correction) stored process CPU deltas
		// in ComputeMs and the CPU-based residual in OverheadMs. Those
		// files are rpc-probe outputs, so their semantics are known:
		// move the CPU value to ProcessCPUMs and clear both admission
		// inputs rather than letting the old labels re-enter a new
		// decision as pure compute / measured overhead.
		for _, m := range []*placement.Measurement{&decCoord, &decWorker} {
			if !m.ProcessCPUMs.Measured && m.ComputeMs.Measured {
				m.ProcessCPUMs = placement.MeasureQS(m.ComputeMs.Value,
					"legacy rpc-probe compute_ms (known process CPU delta, relabeled by the corrected probe)")
			}
			m.ComputeMs = placement.UnmeasuredQ()
			m.OverheadMs = placement.UnmeasuredQ()
		}
		summary["prediction_source"] = "prior run " + *predictFrom
	} else {
		summary["prediction_source"] = "same run (error figure is circular by construction and reported as such)"
	}
	store.Record(decCoord)
	store.Record(decWorker)
	reg := registry.New()
	reg.Register(&registry.Node{ID: "local-coord", Models: []string{*modelID}, LastHeartbeat: now})
	reg.Register(&registry.Node{ID: "local-worker", Models: []string{*modelID}, LastHeartbeat: now})
	a := &placement.Assignment{Kind: "distributed", NodeID: "local-coord",
		Cohort: []placement.CohortMember{{NodeID: "local-worker", RpcAddr: fmt.Sprintf("127.0.0.1:%d", *rpcPort)}}}
	req := placement.Request{ModelID: *modelID}
	rin := placement.InputsFromRegistry(reg, req, a)
	inputs := placement.BuildAdmissionInputs(store, store, req, a)
	placement.MergeRegistryGates(&inputs, rin)
	inputs.RequestID = "rpc-probe-local"
	decision := placement.DecideAdmission(inputs, 10)
	summary["admission_decision"] = decision
	// Prediction-vs-actual is reported ONLY for an independent,
	// condition-controlled cross-run comparison. A same-run figure is
	// circular by construction (the decision was built from this run)
	// and must never be reported as prediction success — not even as
	// an honest-looking 0%.
	summary["prediction_error_pct"] = nil
	switch {
	case *predictFrom == "":
		summary["prediction_source"] = "same run — no independent prediction exists"
		summary["prediction_error_note"] = "not an independent prediction: the decision was built from this run's own measurements, so no prediction error is reported (a same-run figure would be a circular residual fit, not model prediction success)"
	case decision.PredictedJointMs == nil:
		summary["prediction_error_note"] = "the prior-run decision produced no joint prediction (see decision mode/missing), so no error figure exists"
	default:
		priorData, err := os.ReadFile(*predictFrom + "-summary.json")
		if err != nil {
			summary["prediction_error_note"] = "prior run summary could not be read, so its run conditions cannot be verified and no error figure is reported"
			break
		}
		var priorSummary map[string]any
		if err := json.Unmarshal(priorData, &priorSummary); err != nil {
			summary["prediction_error_note"] = "prior run summary does not decode, so its run conditions cannot be verified and no error figure is reported"
			break
		}
		if valid, why := controlledComparison(priorSummary, summary); !valid {
			summary["prediction_error_note"] = "cross-run comparison rejected: " + why
		} else if pct, ok := placement.PredictionErrorPct(decision, wall); ok {
			summary["prediction_error_pct"] = pct
			summary["predicted_joint_ms"] = *decision.PredictedJointMs
			summary["actual_joint_wall_ms"] = wall
			summary["prediction_error_note"] = why
		} else {
			summary["prediction_error_note"] = "no valid prediction error could be computed for the controlled comparison"
		}
	}
	summary["production_verified"] = decision.ProductionVerified
	summary["note"] = "loopback shadow output is not production verification and not WAN evidence; routing is unchanged (shadow). Both processes share this host's CPU, so process-CPU figures are host-local diagnostics, not distributed-performance evidence."

	write := func(path string, v any) {
		data, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
	}
	write(*outPrefix+"-coordinator.json", coord)
	write(*outPrefix+"-worker.json", worker)
	write(*outPrefix+"-summary.json", summary)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(summary)
}
