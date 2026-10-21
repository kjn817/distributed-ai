// Command hidden-verify proves (or refutes) that an activation
// dataset contains true final hidden states.
//
// For every dataset entry it:
//  1. asks a llama-server completion instance for the REAL next-token
//     distribution after the entry's prompt (greedy, n_probs=20 —
//     token IDs and logprobs),
//  2. reconstructs logits from the dataset vector:
//     logits = output.weight · h (+ output.bias if the model has one),
//     streamed from the GGUF in row blocks,
//  3. compares: reconstructed argmax vs the server's chosen token, and
//     |Δlogprob| over the server's top-20 tokens after log-softmax.
//
// If every entry matches within tolerance, the dataset is promoted to
// an exact_hidden_* kind with verified_logits=true and a verification
// record (model SHA-256, server model string, tolerances, errors).
// Otherwise the dataset keeps its candidate kind, the report says by
// how much it failed, and the command exits 1. Nothing is promoted on
// partial success.
//
// The completion instance must be a SEPARATE llama-server from the
// embedding instance (an embedding-mode instance on this build dies on
// chat/completion requests), running the same model file.
//
// Usage:
//
//	hidden-verify -dataset hidden-candidates.json -gguf model.gguf \
//	  -engine-url http://127.0.0.1:18099 -out hidden-verified.json
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"

	"distributed-ai/experiments/progressive"
)

type completionResp struct {
	Model           string `json:"model"`
	TokensEvaluated int    `json:"tokens_evaluated"`
	CompletionProbs []struct {
		ID          int `json:"id"`
		TopLogprobs []struct {
			ID      int     `json:"id"`
			Logprob float64 `json:"logprob"`
		} `json:"top_logprobs"`
	} `json:"completion_probabilities"`
}

type sampleReport struct {
	Prompt         string  `json:"prompt"`
	Position       string  `json:"position"`
	TokensEval     int     `json:"tokens_evaluated"`
	Norm           float64 `json:"norm"`
	ServerChosenID int     `json:"server_chosen_id"`
	ReconArgmaxID  int     `json:"recon_argmax_id"`
	ArgmaxMatch    bool    `json:"argmax_match"`
	MaxTopErr      float64 `json:"max_top_logprob_err"`
	ChosenErr      float64 `json:"chosen_logprob_err"`
	Pass           bool    `json:"pass"`
}

type report struct {
	ModelSHA256  string         `json:"model_sha256"`
	ServerModel  string         `json:"server_model"`
	Tensor       string         `json:"tensor"`
	BiasTensor   bool           `json:"bias_tensor_present"`
	Tolerance    float64        `json:"tolerance_logprob"`
	Samples      []sampleReport `json:"samples"`
	AllPass      bool           `json:"all_pass"`
	PromotedKind string         `json:"promoted_kind,omitempty"`
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func groundTruth(client *http.Client, url, prompt string) (*completionResp, error) {
	body, _ := json.Marshal(map[string]any{
		"prompt": prompt, "n_predict": 1, "temperature": 0, "n_probs": 20,
	})
	resp, err := client.Post(url+"/completion", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/completion: HTTP %d", resp.StatusCode)
	}
	var cr completionResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	if len(cr.CompletionProbs) != 1 || len(cr.CompletionProbs[0].TopLogprobs) == 0 {
		return nil, fmt.Errorf("/completion: no probabilities returned")
	}
	return &cr, nil
}

// reconstructLogits streams output.weight in row blocks and returns
// W·h (+bias when present).
func reconstructLogits(f *os.File, size int64, gf *progressive.GGUFFile, tensor string, bias []float64, h []float64) ([]float64, error) {
	ti, ok := gf.Find(tensor)
	if !ok {
		return nil, fmt.Errorf("tensor %q not found", tensor)
	}
	if len(ti.Dims) != 2 {
		return nil, fmt.Errorf("tensor %q is not 2D: %v", tensor, ti.Dims)
	}
	width, rows := int(ti.Dims[0]), int(ti.Dims[1])
	if len(h) != width {
		return nil, fmt.Errorf("vector width %d != tensor input width %d", len(h), width)
	}
	logits := make([]float64, rows)
	const block = 2048
	for start := 0; start < rows; start += block {
		n := block
		if start+n > rows {
			n = rows - start
		}
		flat, err := gf.ExtractTensorSlice(f, size, tensor, start*width, n*width)
		if err != nil {
			return nil, err
		}
		for r := 0; r < n; r++ {
			row := flat[r*width : (r+1)*width]
			s := 0.0
			for i, w := range row {
				s += w * h[i]
			}
			if bias != nil {
				s += bias[start+r]
			}
			logits[start+r] = s
		}
	}
	return logits, nil
}

func logSoftmax(logits []float64) []float64 {
	m := math.Inf(-1)
	for _, z := range logits {
		if z > m {
			m = z
		}
	}
	sum := 0.0
	for _, z := range logits {
		sum += math.Exp(z - m)
	}
	lse := m + math.Log(sum)
	out := make([]float64, len(logits))
	for i, z := range logits {
		out[i] = z - lse
	}
	return out
}

func main() {
	datasetPath := flag.String("dataset", "", "activation dataset JSON (from hidden-collector)")
	ggufPath := flag.String("gguf", "", "GGUF model file (the same file both servers loaded)")
	tensor := flag.String("tensor", "output.weight", "lm_head tensor name")
	engineURL := flag.String("engine-url", "http://127.0.0.1:18099", "completion llama-server base URL (separate instance, no --embedding)")
	// Tolerance calibration (measured 2026-10-21 on the bundled
	// Qwen2.5-0.5B): true hidden vectors reconstruct with max
	// |Δlogprob| 0.094 over 19×20 top-logprob values (FP32 graph noise
	// across two server instances), while L2-normalized vectors — the
	// failure mode this tool exists to catch — err by 8.56..11.06.
	// The 0.1 default sits at the noise ceiling and ~100× below the
	// failure mode.
	tol := flag.Float64("tolerance", 0.1, "max allowed |Δlogprob| over the server top-20")
	out := flag.String("out", "", "where to write the (possibly promoted) dataset; default: overwrite -dataset")
	reportPath := flag.String("report", "", "also write the verification report JSON to this file (written on success AND failure)")
	flag.Parse()
	if *datasetPath == "" || *ggufPath == "" {
		fmt.Fprintln(os.Stderr, "-dataset and -gguf are required")
		os.Exit(2)
	}
	outPath := *out
	if outPath == "" {
		outPath = *datasetPath
	}
	set, err := progressive.LoadActivations(*datasetPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dataset:", err)
		os.Exit(1)
	}
	if len(set.Prompts) != len(set.Vectors) {
		fmt.Fprintln(os.Stderr, "dataset has no per-vector prompts; cannot verify")
		os.Exit(1)
	}
	f, err := os.Open(*ggufPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	st, _ := f.Stat()
	gf, err := progressive.ParseGGUF(f, st.Size())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gguf:", err)
		os.Exit(1)
	}
	var bias []float64
	biasPresent := false
	if _, ok := gf.Find("output.bias"); ok {
		biasPresent = true
		b, berr := gf.ExtractTensor(f, st.Size(), "output.bias", 0)
		if berr != nil {
			fmt.Fprintln(os.Stderr, "output.bias:", berr)
			os.Exit(1)
		}
		bias = b
	}
	modelHash, err := hashFile(*ggufPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hash:", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	rep := report{ModelSHA256: modelHash, Tensor: *tensor, BiasTensor: biasPresent, Tolerance: *tol, AllPass: true}
	positions := map[string]bool{}
	for i, hvec := range set.Vectors {
		prompt := set.Prompts[i]
		pos := "single"
		if i < len(set.Positions) {
			pos = set.Positions[i]
		}
		positions[pos] = true
		gt, gerr := groundTruth(client, *engineURL, prompt)
		if gerr != nil {
			fmt.Fprintf(os.Stderr, "prompt %q: %v\n", prompt, gerr)
			os.Exit(1)
		}
		rep.ServerModel = gt.Model
		// Tokenization consistency: a "single" entry must evaluate to
		// exactly 1 prompt token on the completion side too.
		if pos == "single" && gt.TokensEvaluated != 1 {
			fmt.Fprintf(os.Stderr, "prompt %q labeled single but completion evaluated %d tokens\n", prompt, gt.TokensEvaluated)
			os.Exit(1)
		}
		logits, rerr := reconstructLogits(f, st.Size(), gf, *tensor, bias, hvec)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "reconstruct:", rerr)
			os.Exit(1)
		}
		lp := logSoftmax(logits)
		argmax := 0
		for j := range logits {
			if logits[j] > logits[argmax] {
				argmax = j
			}
		}
		cp := gt.CompletionProbs[0]
		sr := sampleReport{Prompt: prompt, Position: pos, TokensEval: gt.TokensEvaluated,
			ServerChosenID: cp.ID, ReconArgmaxID: argmax, ArgmaxMatch: argmax == cp.ID}
		if i < len(set.Norms) {
			sr.Norm = set.Norms[i]
		}
		for _, t := range cp.TopLogprobs {
			if t.ID < 0 || t.ID >= len(lp) {
				fmt.Fprintf(os.Stderr, "server token id %d out of range\n", t.ID)
				os.Exit(1)
			}
			e := math.Abs(lp[t.ID] - t.Logprob)
			if e > sr.MaxTopErr {
				sr.MaxTopErr = e
			}
			if t.ID == cp.ID {
				sr.ChosenErr = e
			}
		}
		sr.Pass = sr.ArgmaxMatch && sr.MaxTopErr <= *tol
		if !sr.Pass {
			rep.AllPass = false
		}
		rep.Samples = append(rep.Samples, sr)
		fmt.Fprintf(os.Stderr, "%q pos=%s chosen=%d recon=%d match=%v maxTopErr=%.4f chosenErr=%.4f pass=%v\n",
			prompt, pos, sr.ServerChosenID, sr.ReconArgmaxID, sr.ArgmaxMatch, sr.MaxTopErr, sr.ChosenErr, sr.Pass)
	}

	if rep.AllPass {
		switch {
		case len(positions) == 1 && positions["single"]:
			rep.PromotedKind = progressive.KindExactHiddenSingle
		case len(positions) == 1 && positions["last"]:
			rep.PromotedKind = progressive.KindExactHiddenContextual
		default:
			fmt.Fprintln(os.Stderr, "mixed positions: refusing to promote under a single kind; collect single/contextual separately")
			os.Exit(1)
		}
		set.Kind = rep.PromotedKind
		set.VerifiedLogits = true
		maxErr := 0.0
		for _, s := range rep.Samples {
			if s.MaxTopErr > maxErr {
				maxErr = s.MaxTopErr
			}
		}
		set.Verification = fmt.Sprintf("hidden-verify: reconstructed logits (output.weight × h, bias=%v) vs llama-server /completion top-20 logprobs; %d/%d samples argmax match, max |Δlogprob| %.4f <= tol %.2f; model sha256 %s; server model %q",
			biasPresent, len(rep.Samples), len(rep.Samples), maxErr, *tol, modelHash, rep.ServerModel)
		if err := progressive.SaveActivations(outPath, set); err != nil {
			fmt.Fprintln(os.Stderr, "save:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "PROMOTED to %s -> %s\n", rep.PromotedKind, outPath)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	if *reportPath != "" {
		data, _ := json.MarshalIndent(rep, "", "  ")
		_ = os.WriteFile(*reportPath, append(data, '\n'), 0o600)
	}
	if !rep.AllPass {
		fmt.Fprintln(os.Stderr, "VERIFICATION FAILED — dataset NOT promoted")
		os.Exit(1)
	}
}
