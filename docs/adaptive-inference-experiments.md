# Adaptive Inference experiments (branch: experiment/adaptive-inference)

Two experiment features, both **off the production path by default** and
neither merged to `main` nor deployed. This document records what each
one is, what it is not, and how it was verified.

## 1. Adaptive Compute Admission — `service/internal/placement/admission.go`

A measurement-driven comparison of single-node vs joint execution,
integrated into the placement package in **shadow mode**:

- `DecideAdmission` is a pure function. Every input quantity carries a
  `Measured` flag; anything unmeasured or invalid degrades the decision
  to `insufficient_data` with the missing fields listed. Estimates are
  never presented as measurements.
- Modes: `single`, `distributed_candidate` (a prediction, never
  execution evidence), `insufficient_data`.
- Joint prediction: `coordinator + worker + RTT × rounds + payload
  transfer + overhead`, gated by three proof booleans
  (`rpc_reachable`, `model_ready`, `joint_execution_safe`).
- The API records decisions after normal placement only when enabled
  (`ADMISSION_SHADOW=1` in `service/cmd/server`, or
  `Server.WithAdmissionShadow`). Default OFF: no recording, the shadow
  endpoint answers 404, chat routing is unchanged. Decisions are
  inspectable at `GET /v1/experiments/admission/shadow` when enabled.

**Honest current state:** the registry has no per-node inference timings
and no bandwidth measurements, and the service cannot verify
coordinator→worker RPC reachability (`InputsFromRegistry` says all of
this in its notes). So on the live path today every recorded decision is
`insufficient_data`. That is the correct result until a real measurement
source is wired in — the algorithm refuses to guess.

## 2. Certified Progressive Inference — `experiments/progressive/`

Offline Go module for certified activation quantization of **one affine
layer**: INT4/INT8/FP16 comparison, argmax match rate, certified rate
(pairwise error bound), compression ratio, and a minimal GGUF reader
(F32/F16/Q8_0) so real model weights can feed the experiment.

Not implemented, and reported as such in every JSON report:
transformer-wide guarantees, progressive bitplane refinement, extra-bit
transmission and its RTT cost. See `experiments/progressive/README.md`.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` — see the test run
  log captured with this change.
- Unit + boundary tests cover: threshold boundaries, every input
  unmeasured/invalid, each gate closed, FP16 known values, quantization
  bound soundness, certificate soundness over random trials, GGUF
  fixture parse/extract, and a real-model GGUF parse/extract when the
  bundled model file is present.

## Explicit non-results

- No joint inference was executed; no Phase B run; nothing was deployed;
  Oracle services were not restarted; the credit ledger is untouched.
- `distributed_candidate` has never been produced from live registry
  data (inputs are unmeasured there by design).
