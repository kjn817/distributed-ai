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

# run with real weights from a tensor (activations stay synthetic —
# no activation-capture path exists yet, and the report says so)
go run ./experiments/progressive/cmd/progressive-demo \
  -gguf models/qwen2.5-0.5b-instruct-q4_k_m.gguf -tensor output.weight
```

## Origin

Ported and hardened from the 2026-10-21 Research Lab proof-of-concept
(Python `progressive_lab.py`), re-implemented in Go inside the DAIN
module so it is covered by `go test ./...`. Differences from the PoC:
FP16 baseline added, bias support, dimension validation, a real GGUF
extraction path, and the certificate invariant (certified ⇒ matched) is
enforced inside `RunExperiment` itself, not only in tests.
