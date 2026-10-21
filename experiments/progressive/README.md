# Certified Progressive Inference — offline experiment

Status: **experiment only.** Not imported by the production service, not
deployed, and not a claim about the full Qwen/Transformer model.

## What this module does

For a **single affine layer** `z = W·x + b`, it quantizes the activation
vector `x` at INT4 / INT8 / FP16 and measures:

- **argmax match rate** — how often the quantized-input argmax equals the
  FP reference argmax (observed).
- **certified rate** — how often a pairwise worst-case error bound proves
  the argmax cannot change: for winner `w` and every competitor `j`,
  `z_w - z_j > Σ_i |W[w][i] - W[j][i]| · errBound`. Certified samples are
  guaranteed to match; the test suite treats any violation as a failure.
- **compression ratio** — FP32 wire bytes vs quantized bytes per vector,
  including the 4-byte scale for INT4/INT8.
- **error bound** — the a-priori bound actually used by the certificate
  (`scale/2` for symmetric quantization, max half-ULP/2 for FP16).

## What it does NOT do

- **No Transformer-wide guarantee.** Propagating a sound bound through
  attention, norms and stacked layers is unimplemented. Every report
  carries `guarantee_scope: "single_affine_layer_only"` and
  `transformer_wide_guarantee: false`.
- **No progressive bitplane streaming.** INT4 and INT8 are independent
  quantizations, not refinement stages. No extra bits are transmitted by
  this code; reports carry `progressive_refinement_implemented: false`,
  `extra_bits_transmitted: 0`, and `extra_rtt_cost_ms: null` (not
  measured — because there is nothing to measure yet).
- **No llama.cpp integration.** The GGUF reader exists only to extract
  real weights for the experiment.

## Real-data path

`gguf.go` is a minimal GGUF reader (header, metadata, tensor directory)
with extraction for **F32, F16 and Q8_0** payloads. Other payload types
(Q4_K etc.) are refused with `ErrUnsupportedTensorType` rather than
decoded incorrectly.

```sh
# synthetic weights (report labels them synthetic)
go run ./experiments/progressive/cmd/progressive-demo

# list tensors of a real model
go run ./experiments/progressive/cmd/progressive-demo \
  -gguf models/qwen2.5-0.5b-instruct-q4_k_m.gguf -list

# real weights + synthetic activations
go run ./experiments/progressive/cmd/progressive-demo \
  -gguf models/qwen2.5-0.5b-instruct-q4_k_m.gguf -tensor output.weight
```

Real hidden activations are collected without modifying llama.cpp:
`cmd/hidden-collector` uses the server's `/v1/embeddings` endpoint
(start the server with `--embedding --pooling mean`). For a prompt of
exactly one token, pooling is the identity, so the returned vector is
the final hidden state — the exact input of `output.weight`. The
collector verifies the token count from the response usage and skips
multi-token prompts; datasets are kind-labeled
(`exact_hidden_single_token` vs `pooled_multi_token` vs `synthetic`)
and the demo refuses pooled data unless `-allow-pooled` is passed.
Datasets themselves are experiment data and are not committed to the
repository.

```sh
# collect real hidden activations (separate llama-server instance with
# --embedding --pooling mean), then real weights + real activations
go run ./experiments/progressive/cmd/hidden-collector \
  -engine-url http://127.0.0.1:18100 \
  -prompts "Hello,The,Water,Fire,Paris,king,queen,cat" \
  -model qwen2.5-0.5b-instruct -out /tmp/hidden.json
go run ./experiments/progressive/cmd/progressive-demo \
  -gguf models/qwen2.5-0.5b-instruct-q4_k_m.gguf -tensor output.weight \
  -activations /tmp/hidden.json -full-vocab
```

Measured on 2026-10-21 (19 single-token prompts, full 151,936-row
vocabulary): INT4 match 26.3% / certified 0%, INT8 match 100% /
certified 0%, FP16 match 100% / certified 78.9%. Real activations are
far less forgiving than Gaussian ones (compare the synthetic runs in
the docs) — and at vocabulary scale the pairwise bound is too
pessimistic to certify INT8 despite a perfect match rate. Both facts
are properties of the method on real data, recorded as-is.

## Origin

Ported and hardened from the 2026-10-21 Research Lab proof-of-concept
(Python `progressive_lab.py`), re-implemented in Go inside the DAIN
module so it is covered by `go test ./...`. Differences from the PoC:
FP16 baseline added, bias support, dimension validation, a real GGUF
extraction path, and the certificate invariant (certified ⇒ matched) is
enforced inside `RunExperiment` itself, not only in tests.
