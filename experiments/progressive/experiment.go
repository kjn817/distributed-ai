package progressive

import (
	"fmt"
	"math/rand"
)

// BitsReport summarizes one precision over the whole sample set.
type BitsReport struct {
	Bits int `json:"bits"`
	// ArgmaxMatchRate: fraction of samples where the quantized-input
	// argmax equaled the FP reference argmax (observed, not proven).
	ArgmaxMatchRate float64 `json:"argmax_match_rate"`
	// CertifiedRate: fraction of samples where the pairwise bound
	// certified the argmax. Certified is always a subset of matched;
	// RunExperiment returns an error if that invariant is ever broken.
	CertifiedRate float64 `json:"certified_rate"`
	// CompressionRatio: FP32 wire bytes / quantized wire bytes for one
	// activation vector, scale overhead included.
	CompressionRatio float64 `json:"compression_ratio"`
	// ActivationBytes is the per-vector wire size at this precision.
	ActivationBytes int `json:"activation_bytes"`
	// MeanErrBound is the mean MaxErrBound across samples.
	MeanErrBound float64 `json:"mean_err_bound"`
}

// Report is the full experiment record. Its honesty fields are part of
// the API: a consumer must be able to see what was NOT done.
type Report struct {
	Experiment string `json:"experiment"`
	// GuaranteeScope is deliberately narrow and constant.
	GuaranteeScope           string       `json:"guarantee_scope"`
	TransformerWideGuarantee bool         `json:"transformer_wide_guarantee"`
	DataSource               string       `json:"data_source"`
	ActivationSource         string       `json:"activation_source"`
	Samples                  int          `json:"samples"`
	Width                    int          `json:"width"`
	Classes                  int          `json:"classes"`
	Results                  []BitsReport `json:"results"`

	// Progressive refinement (send INT4, then refine with extra bits)
	// is NOT implemented in this module. These fields exist so the
	// report states that plainly instead of implying success:
	ProgressiveRefinementImplemented bool     `json:"progressive_refinement_implemented"`
	ExtraBitsTransmitted             int      `json:"extra_bits_transmitted"`
	ExtraRTTCostMs                   *float64 `json:"extra_rtt_cost_ms"` // nil = not measured (not implemented)
	Notes                            []string `json:"notes"`
}

const guaranteeScope = "single_affine_layer_only"

// RunExperiment evaluates INT4/INT8/FP16 over the given weights and
// activation samples. It returns an error if a certified sample ever
// disagrees with the reference (that would invalidate the certificate).
func RunExperiment(W [][]float64, activations [][]float64, dataSource, activationSource string) (Report, error) {
	if len(W) == 0 || len(activations) == 0 {
		return Report{}, ErrEmptyLayer
	}
	width := len(W[0])
	rep := Report{
		Experiment:                       "certified activation quantization — single affine layer",
		GuaranteeScope:                   guaranteeScope,
		TransformerWideGuarantee:         false,
		DataSource:                       dataSource,
		ActivationSource:                 activationSource,
		Samples:                          len(activations),
		Width:                            width,
		Classes:                          len(W),
		ProgressiveRefinementImplemented: false,
		ExtraBitsTransmitted:             0,
		ExtraRTTCostMs:                   nil,
		Notes: []string{
			"INT4/INT8/FP16 are independent quantizations of the same vector, not stages of a progressive stream",
			"progressive bitplane refinement is not implemented; no extra bits were transmitted and no extra RTT was measured",
			"the certificate covers this one affine layer only; no claim is made about a full Transformer",
			"compression ratio compares one activation vector against FP32 and includes the 4-byte scale (INT4/INT8)",
		},
	}
	for _, bits := range []int{4, 8, 16} {
		var matched, certified int
		var errBoundSum float64
		var actBytes int
		for _, x := range activations {
			if len(x) != width {
				return Report{}, fmt.Errorf("%w: activation len %d vs width %d", ErrDimensionMismatch, len(x), width)
			}
			res, err := CertifyAffine(W, nil, x, bits)
			if err != nil {
				return Report{}, err
			}
			if res.Certified && !res.Match {
				return Report{}, fmt.Errorf("progressive: CERTIFICATE VIOLATED at %d bits: certified sample disagrees with reference", bits)
			}
			if res.Match {
				matched++
			}
			if res.Certified {
				certified++
			}
			errBoundSum += res.Quantized.MaxErrBound
			actBytes = res.Quantized.PayloadBytes
		}
		br := BitsReport{
			Bits:            bits,
			ArgmaxMatchRate: float64(matched) / float64(len(activations)),
			CertifiedRate:   float64(certified) / float64(len(activations)),
			ActivationBytes: actBytes,
			MeanErrBound:    errBoundSum / float64(len(activations)),
		}
		if actBytes > 0 {
			br.CompressionRatio = float64(width*4) / float64(actBytes)
		}
		rep.Results = append(rep.Results, br)
	}
	return rep, nil
}

// RunSynthetic runs the experiment on seeded Gaussian weights and
// activations. Everything about it is synthetic and the report says so;
// it exists to exercise the machinery, not to measure a real model.
func RunSynthetic(seed int64, samples, width, classes int) (Report, error) {
	if samples <= 0 || width <= 0 || classes <= 0 {
		return Report{}, fmt.Errorf("progressive: samples, width and classes must be positive")
	}
	rng := rand.New(rand.NewSource(seed))
	W := make([][]float64, classes)
	for i := range W {
		W[i] = make([]float64, width)
		for j := range W[i] {
			W[i][j] = rng.NormFloat64()
		}
	}
	acts := make([][]float64, samples)
	for i := range acts {
		acts[i] = make([]float64, width)
		for j := range acts[i] {
			acts[i][j] = rng.NormFloat64()
		}
	}
	return RunExperiment(W, acts,
		"synthetic Gaussian weights (seeded) — not a real model",
		"synthetic Gaussian activations (seeded)")
}

// WeightsFromTensor reshapes a flat extracted tensor into a weight
// matrix. GGUF stores dims innermost-first, so a 2D tensor with
// dims [in, out] holds `out` rows of `in` values in row-major order.
// maxClasses caps the rows used (the first ones).
func WeightsFromTensor(flat []float64, dims []uint64, maxClasses int) ([][]float64, error) {
	if len(dims) != 2 {
		return nil, fmt.Errorf("progressive: need a 2D tensor, got dims %v", dims)
	}
	in, out := int(dims[0]), int(dims[1])
	if in <= 0 || out <= 0 || len(flat) < in*out {
		return nil, fmt.Errorf("progressive: tensor too small: %d values for %dx%d", len(flat), in, out)
	}
	rows := out
	if maxClasses > 0 && maxClasses < rows {
		rows = maxClasses
	}
	W := make([][]float64, rows)
	for i := 0; i < rows; i++ {
		row := make([]float64, in)
		copy(row, flat[i*in:(i+1)*in])
		W[i] = row
	}
	return W, nil
}

// SyntheticActivations generates deterministic seeded activation vectors
// of the given width. Used with real GGUF weights when no captured
// activation set exists — reports label them synthetic.
func SyntheticActivations(seed int64, samples, width int) [][]float64 {
	rng := rand.New(rand.NewSource(seed))
	acts := make([][]float64, samples)
	for i := range acts {
		acts[i] = make([]float64, width)
		for j := range acts[i] {
			acts[i][j] = rng.NormFloat64()
		}
	}
	return acts
}
