# Adaptive Inference experiments (branch: experiment/adaptive-inference)

Two experiment features, both **off the production path by default** and
neither merged to `main` nor deployed. This document records what each
one is, what it is not, how it was verified, and how to reproduce the
measurements. Round 2 (2026-10-21) hardened the round-1 code and added
the real-data paths; see "Round 2 changes" below.

## 1. Adaptive Compute Admission — `service/internal/placement/admission.go`

A measurement-driven comparison of single-node vs joint execution,
integrated into the placement package in **shadow mode**:

- `DecideAdmission` is a pure function. Every input quantity carries a
  `Measured` flag and a `Source`; anything unmeasured or invalid
  degrades the decision to `insufficient_data` with the missing fields
  listed. Estimates are never presented as measurements.
- Modes: `single`, `distributed_candidate` (a prediction, never
  execution evidence), `insufficient_data`.
- Joint prediction: `coordinator + worker + RTT × rounds + payload
  transfer + overhead`. Timing scopes are defined in
  `placement.MeasurementScopes` and embedded in every decision JSON:
  compute times are pure compute, network time is priced once
  (RTT × rounds + derived transfer), overhead is the residual only —
  so network/sync time cannot be double-counted by construction of the
  formula, and collectors are contract-bound to the same scopes.
- The API records decisions after normal placement only when enabled
  (`ADMISSION_SHADOW=1` in `service/cmd/server`, or
  `Server.WithAdmissionShadow`). Default OFF: no recording, chat
  routing is unchanged. Decisions are inspectable at
  `GET /v1/experiments/admission/shadow` when enabled.

### Eligibility tiers (round 2)

Round 1 required `joint_execution_safe` proof before any candidate
could be produced — a circular dependency, since the proof can only
come from running the experiment the gate was blocking. Round 2 splits
the tiers:

- **Experimental eligibility** (`experimental_eligible`): data complete,
  `rpc_reachable` verified, `model_ready` verified **for every
  participant** (coordinator and each cohort worker; round 1 checked a
  single node). An eligible candidate may be run as a controlled
  experiment. Nothing more.
- **Production verification** (`production_verified`): additionally
  requires a `ProofRecord` with an evidence reference
  (`joint_execution_proven` + `proof_evidence`). A bare boolean claim
  is rejected by `DecideAdmission`. Only production-verified
  candidates may ever feed a production policy; shadow mode assigns
  nothing regardless of tier.

### Shadow endpoint access control (round 2)

Recorded decisions are operational data; the endpoint is not public:

- shadow OFF → `404`; shadow ON without `ADMISSION_ADMIN_KEY` → `403`
  (endpoint closed); missing/wrong `Authorization: Bearer <key>` →
  `401` (constant-time comparison); correct key → `200`.
- The payload contains only measurements, generated request IDs, gate
  states and scope definitions. Chat content, API keys, node network
  addresses and source IPs are never recorded in the first place; a
  regression test asserts their absence from the response.

### Measurement interface (round 2) — `placement/measurement.go`

`Measurement` records one observed run with: model ID, **SHA-256 of
the actual weights file** (`HashWeightsFile`), role
(single/coordinator/worker), wall time, engine prefill/decode timings,
token counts and tokens/s, pure-compute time (scope-bound), same-path
RTT/bandwidth with a `NetworkPath` identifier, RPC-connected flags,
actual tensor bytes, outcome and error type. `MeasurementStore` /
`ProofSource` are interfaces; `MemoryMeasurementStore` is the bounded
in-memory reference implementation. `BuildAdmissionInputs` combines
records under strict rules: failed runs are ignored, network
quantities combine only when coordinator and worker records share a
`NetworkPath`, `rpc_reachable` needs both ends connected, multi-worker
cohorts are not aggregated in v1 (worker time stays unmeasured), and
production proof comes only from a `ProofRecord` — never from timing
data.

`service/cmd/admission-probe` collects a real single-node measurement
from any running llama-server (wall clock by the probe, phase timings
from the engine's own `timings` report; anything unreported stays
unmeasured).

**Real measurement, captured 2026-10-21 in the local VM (independent
test environment, not Oracle):** llama-server + Qwen2.5-0.5B Q4_K_M
(weights sha256 `74a4da8c9fdb…`), prompt "Explain gravity in one
sentence.": wall 1841.1 ms, prefill 445.3 ms, decode 1332.5 ms,
36 prompt + 28 generated tokens, 20.26 tok/s. Fed through
collect → store → build → decide (`TestRealMeasurementFilePipeline`,
runs when `ADMISSION_MEASUREMENT_JSON` points at the probe output),
the baseline becomes measured and the decision is still
`insufficient_data` — the joint side has no measurements, and none are
invented. External Mac↔Oracle joint execution remains unverified, so
no live `distributed_candidate` has ever been produced from real joint
data.

## 2. Certified Progressive Inference — `experiments/progressive/`

Offline Go module for certified activation quantization of **one affine
layer**: INT4/INT8/FP16 comparison, argmax match rate, certified rate
(pairwise error bound), compression ratio, and a minimal GGUF reader
(F32/F16/Q8_0) for real model weights.

### Real hidden activations (round 2)

`experiments/progressive/cmd/hidden-collector` collects real hidden
states **without modifying llama.cpp**: a llama-server started with
`--embedding --pooling mean` serves `/v1/embeddings`; for a
single-token prompt, pooling is the identity, so the vector is the
final hidden state — the exact input of `output.weight`. Token counts
are verified from response usage; multi-token prompts are skipped (or
explicitly labeled `pooled_multi_token` with `-allow-pooled`, which
the demo refuses by default). No engine patch was needed; if a future
model/server cannot serve embeddings this way, the fallback is an
experiment-only llama.cpp patch — to be proposed and scoped before any
engine change, per project rules.

Results, 2026-10-21, real `output.weight` (Q8_0), 19 real hidden
vectors (single-token prompts):

| scope | INT4 match/cert | INT8 match/cert | FP16 match/cert |
|---|---|---|---|
| full vocabulary (151,936 rows) | 26.3% / 0% | 100% / 0% | 100% / 78.9% |
| first 128 rows only | 36.8% / 0% | 100% / 0% | 100% / 68.4% |

Compare round 1 (synthetic Gaussian activations, 128 rows): INT4 match
was 84.5%. Real activations are far less forgiving to per-tensor
symmetric INT4, and at vocabulary scale the pairwise bound certifies
nothing at INT8 despite a perfect match rate — the bound, not the
prediction, is the bottleneck. Subset (128-row) numbers must not be
quoted as vocabulary results; the demo labels the row scope in every
report.

Failure cases are tested explicitly: an INT4 argmax flip is detected
(match=false, certified=false), FP16 overflow and non-finite inputs
error out instead of certifying, and `RunExperiment` itself errors if
a certified sample ever disagrees with the reference.

Not implemented, and reported as such in every JSON report:
transformer-wide guarantees, progressive bitplane refinement,
extra-bit transmission and its RTT cost. See
`experiments/progressive/README.md`.

## Verification

- Round 1 evidence: `docs/adaptive-inference-test-output-20261021.txt`.
  **Correction:** that run used Go's default test caching — several
  packages reported `(cached)`. It proved the code compiled and the
  uncached packages passed, but it was not a full fresh execution.
- Round 2 evidence: `docs/adaptive-inference-test-output-20261021-r2.txt`,
  produced with `go build ./...`, `go vet ./...` and
  **`go test -count=1 ./...`** (cache disabled), plus the probe
  measurement JSONs and the real-activation demo outputs.
- Reproduction: start the bundled llama-server locally
  (`engines/llama.cpp/build/bin/llama-server -m
  models/qwen2.5-0.5b-instruct-q4_k_m.gguf --port 18099`), run
  `service/cmd/admission-probe` for measurements; start a second
  instance with `--embedding --pooling mean` on port 18100 and run
  `hidden-collector` + `progressive-demo -activations … -full-vocab`
  for the activation experiment. The model file is a local artifact
  and is never committed.
- Environment note: on this VM, a llama-server started with
  `--embedding` died when a chat completion was requested from it;
  the embedding instance must serve embeddings only (separate
  instances per role). Recorded here so nobody rediscovers it.

## Round 3 (2026-10-21) — hidden-state correction + RPC measurement

### Hidden-state accuracy: the round-2 label was wrong; the numbers survive

Round 2 collected activations via `/v1/embeddings` with
`--pooling mean` and labeled them `exact_hidden_single_token`. That
label was **wrong**: llama.cpp's `embd_normalize` defaults to 2 (L2)
in the bundled build (commit `10b8b7553494f3638da7668cf17cac089229122a`,
b11712 — confirmed in `common/common.h`), so those vectors were
L2-normalized direction vectors, not hidden states.

Round 3 establishes the verified path (no llama.cpp modification):

- A dedicated instance with `--embedding --pooling none
  --embd-normalize -1` serves the native `/embedding` endpoint:
  per-token raw vectors (norms ≈ 230–290, not 1).
- `cmd/hidden-verify` reconstructs logits (`output.weight · h`; this
  model has no `output.bias`) and compares them against a separate
  completion instance's real top-20 logprobs. True vectors: max
  |Δlogprob| 0.094, argmax 19/19. The normalized counterfactual errs
  by 8.56–11.06 — the check discriminates by ~100×.
- Single-token datasets are promoted to `exact_hidden_single_token`
  only on a full pass (label discipline is enforced in
  `ActivationSet.Validate`). Contextual last-token vectors behave as
  true hidden states at the head of the distribution (chosen-token
  logprob error ≤ 0.08, 4/5 argmax, one near-tie flip) but fail the
  strict top-20 tolerance (max 0.26 even single-threaded), so they
  remain `hidden_candidate_unverified` — not promoted, not hidden
  behind a loosened threshold.
- Re-running the experiment on verified raw hidden states reproduces
  the round-2 rates exactly (full vocab INT4 26.3%/0%, INT8 100%/0%,
  FP16 100%/78.9%): per-sample positive scaling changes neither argmax
  nor the scale-equivariant symmetric-quantization certificate. The
  numbers were right; their provenance label was not. Both facts are
  recorded.

### RPC path measurement (isolated loopback) + admission connection

`service/cmd/rpc-probe` measures a real ggml-rpc run locally:
process CPU time per role (diagnostic only — see the reporting
corrections below), TCP-handshake RTT, bulk loopback
bandwidth, loopback byte deltas (tensor + protocol), protocol-known
round count, residual overhead, weights hash, layer placement
(-ngl 99). Findings, recorded as-is:

- Multi-threaded ggml-rpc worker CPU time (2510 ms) exceeds the wall
  time (1922 ms) — thread spin makes CPU deltas an invalid
  pure-compute proxy there; with `-t 1` on both sides the accounting
  closes (wall 1363 ms = coord 20 + worker 1230 + network ≈6 +
  residual overhead 107).
- With measured inputs the shadow decision on loopback is
  `distributed_candidate`, `experimental_eligible=true`,
  `production_verified=false` — as designed.
- Prediction-vs-actual (`placement.PredictionErrorPct`): a same-run
  comparison is circular (the residual definition forces 0%) and is
  labeled as such. Cross-run, the prior run's prediction (1363 ms)
  vs fresh actuals (435 ms, 628 ms) errs by 213% / 117% — dominated
  by shared-VM CPU variance and prompt-cache reuse, i.e. loopback
  CPU-time inputs on this VM are too noisy for tight prediction. The
  recording machinery works; the error figures are methodology data,
  not WAN or algorithm-quality evidence.
- Path mixing is prevented by the `NetworkPath` equality rule in
  `BuildAdmissionInputs`; every record here carries
  `local-loopback-ggml-rpc (NOT WAN evidence)`.

### Round 3 reporting corrections (2026-10-21, later the same day)

The reporting rules for this experiment were tightened after the
round-3 measurements. These corrections supersede the labels — not
the raw observations — in the section above:

- **Process CPU time is not pure compute.** `Measurement` now carries
  a separate `process_cpu_ms` quantity. `rpc-probe` records its
  `/proc` utime+stime deltas there and leaves `compute_ms` and
  `overhead_ms` unmeasured; `BuildAdmissionInputs` deliberately
  ignores `process_cpu_ms`. Legacy rpc-probe files fed back through
  `-predict-from` are relabeled the same way before use, so the old
  CPU-as-compute values cannot re-enter a decision.
- **The `-t 1` “closure” is an accounting identity, not a compute
  proof.** wall = coordinator CPU + worker CPU + network estimate +
  residual closes arithmetically over process-CPU deltas. It does not
  establish pure per-role compute, and the residual is no longer
  recorded as measured overhead. Consequently the round-3 loopback
  `distributed_candidate` — produced under the superseded labels — is
  **withdrawn as a prediction result**: the collection pipeline was
  demonstrated, but with corrected semantics rpc-probe records alone
  yield `insufficient_data` (missing pure coordinator/worker compute
  and overhead), which is the honest state.
- **No same-run prediction error.** A decision built from a run
  cannot be scored against that same run (the residual construction
  makes the figure circular). `rpc-probe` now emits
  `prediction_error_pct: null` with the reason for same-run use.
  Cross-run errors are emitted only when the comparison is verified
  controlled: same prompt (by SHA-256), same token cap,
  `cache_prompt=false` in both runs, and the same actual
  generated-token count. The probe now sends `cache_prompt: false`
  and records all four conditions in its summary. Pre-correction
  summaries lack these fields and are therefore incomparable — the
  historical 213% / 117% figures remain methodology notes about
  uncontrolled loopback variance, not validated model errors.
- **`-ngl 99` is full offload, not joint layer compute.** All layers
  sit on the single RPC worker. It is not evidence that a coordinator
  CPU and a worker GPU split layers or jointly computed anything, and
  summaries now carry `joint_compute_verified: false` with that
  interpretation stated.
- **Contextual hidden state remains unverified.** Nothing in this
  correction promotes it: it stays `hidden_candidate_unverified`
  under the strict top-20 criterion.

### Phase A networking

The libp2p Relay + NAT Traversal direction from round 1 stands.
`docs/phase-a-relay-minimal-plan.md` turns the review into a minimal
implementation plan (new `node/cmd/relay`, host options, reporter ACK
+ PeerID binding, direct/relayed log discipline, acceptance tests).
Nothing is built or deployed by that document; relay hosting/domain
decisions and any Oracle change remain separately gated.

## Explicit non-results

- No joint inference was executed; no Phase B run; nothing was
  deployed; Oracle services were not restarted; the credit ledger is
  untouched; C (distributed KV) and D (contribution verification) were
  not implemented in either round.
- `distributed_candidate` has never been produced from live registry
  or joint measurement data (inputs are unmeasured there by design).
