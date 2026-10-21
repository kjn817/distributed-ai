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

Real hidden activations are collected without modifying llama.cpp —
but the obvious path is a trap: `/v1/embeddings` output is
**L2-normalized by default** (`embd_normalize=2` in llama.cpp), so
those vectors are direction-only embeddings, NOT hidden states. An
earlier revision of this module made exactly that mistake; the label
has been corrected and the data re-collected properly:

- `cmd/hidden-collector` uses a dedicated instance started with
  `--embedding --pooling none --embd-normalize -1` and the native
  `/embedding` endpoint, which returns raw per-token vectors. It
  records prompts, positions, and L2 norms, and labels the output
  `hidden_candidate_unverified` (or `embedding_l2_normalized` if the
  norms come back ≈ 1 anyway).
- `cmd/hidden-verify` is the only path to an `exact_hidden_*` label:
  it reconstructs logits from each vector (`output.weight · h`, plus
  `output.bias` when a model has one) and compares them with a
  separate completion instance's real top-20 logprobs. Measured on
  the bundled Qwen2.5-0.5B: true vectors err ≤ 0.094; normalized
  vectors err 8.56–11.06. `ActivationSet.Validate` rejects an
  exact-hidden label without a recorded verification, so the
  round-2 labeling bug is now structurally impossible.
- Contextual (last-token) collection works, but those vectors have
  not passed the strict verification tolerance and stay candidates.

Datasets themselves are experiment data and are not committed to the
repository.

```sh
# collect candidate hidden activations (dedicated instance with
# --embedding --pooling none --embd-normalize -1), verify them against
# real logits (separate completion instance on :18099), then run
go run ./experiments/progressive/cmd/hidden-collector \
  -engine-url http://127.0.0.1:18100 \
  -prompts "Hello,The,Water,Fire,Paris,king,queen,cat" \
  -model qwen2.5-0.5b-instruct -out /tmp/hidden-candidates.json
go run ./experiments/progressive/cmd/hidden-verify \
  -dataset /tmp/hidden-candidates.json \
  -gguf models/qwen2.5-0.5b-instruct-q4_k_m.gguf \
  -engine-url http://127.0.0.1:18099 -out /tmp/hidden-verified.json
go run ./experiments/progressive/cmd/progressive-demo \
  -gguf models/qwen2.5-0.5b-instruct-q4_k_m.gguf -tensor output.weight \
  -activations /tmp/hidden-verified.json -full-vocab
```

Measured on 2026-10-21 (19 single-token prompts, logit-verified raw
hidden states, full 151,936-row vocabulary): INT4 match 26.3% /
certified 0%, INT8 match 100% / certified 0%, FP16 match 100% /
certified 78.9%. Real activations are
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
