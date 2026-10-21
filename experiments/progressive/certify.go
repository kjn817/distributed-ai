package progressive

import (
	"errors"
	"fmt"
	"math"
)

// LayerResult is the outcome of running one activation vector through
// one affine layer at one precision.
type LayerResult struct {
	// RefArgmax is the argmax of the FP64 reference logits W·x+b.
	RefArgmax int
	// PredArgmax is the argmax of the logits from the quantized input.
	PredArgmax int
	// Match: PredArgmax == RefArgmax (observed for this sample).
	Match bool
	// Certified: the pairwise error bound proves the argmax cannot
	// differ from the quantized computation's winner for ANY input
	// within the quantization error bound — and therefore the winner
	// equals the reference argmax. Certified implies Match; the
	// experiment asserts this implication on every sample.
	Certified bool
	// Margin is min over competitors j of (z_w - z_j - bound_j) for the
	// quantized winner w. Certified iff Margin > 0.
	Margin float64
	// Quantized carries the quantization details (bound, wire size).
	Quantized Quantized
}

var (
	ErrDimensionMismatch = errors.New("progressive: dimension mismatch")
	ErrEmptyLayer        = errors.New("progressive: empty weights or input")
)

// dot is a plain FP64 dot product.
func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// argmax returns the index of the maximum (first index on ties).
func argmax(v []float64) int {
	best := 0
	for i := 1; i < len(v); i++ {
		if v[i] > v[best] {
			best = i
		}
	}
	return best
}

// CertifyAffine runs the single-layer experiment for one sample:
//
//  1. reference logits from the exact input x,
//  2. quantized-input logits from Quantize(x, bits),
//  3. certificate: for the quantized winner w and every competitor j,
//     the logit difference z_w - z_j can move by at most
//     sum_i |W[w][i]-W[j][i]| * errBound when the input is perturbed
//     within the quantization bound. If z_w - z_j exceeds that bound for
//     all j, no input within the bound — including the exact x — can
//     flip the winner, so the reference argmax is certified equal.
//
// The bound is sound for this affine map only. It says nothing about a
// stack of layers: each additional layer would need its own propagated
// bound, which this package does not implement.
func CertifyAffine(W [][]float64, b []float64, x []float64, bits int) (LayerResult, error) {
	if len(W) == 0 || len(x) == 0 {
		return LayerResult{}, ErrEmptyLayer
	}
	if b != nil && len(b) != len(W) {
		return LayerResult{}, fmt.Errorf("%w: bias len %d vs classes %d", ErrDimensionMismatch, len(b), len(W))
	}
	for i, row := range W {
		if len(row) != len(x) {
			return LayerResult{}, fmt.Errorf("%w: row %d len %d vs input %d", ErrDimensionMismatch, i, len(row), len(x))
		}
	}
	bias := func(i int) float64 {
		if b == nil {
			return 0
		}
		return b[i]
	}

	q, err := Quantize(x, bits)
	if err != nil {
		return LayerResult{}, err
	}

	ref := make([]float64, len(W))
	pred := make([]float64, len(W))
	for i, row := range W {
		ref[i] = dot(row, x) + bias(i)
		pred[i] = dot(row, q.Dequantized) + bias(i)
	}
	refWin := argmax(ref)
	predWin := argmax(pred)

	res := LayerResult{
		RefArgmax:  refWin,
		PredArgmax: predWin,
		Match:      refWin == predWin,
		Quantized:  q,
	}
	if q.MaxErrBound == 0 {
		// Exact representation (e.g. zero vector): trivially certified.
		res.Certified = res.Match
		res.Margin = math.Inf(1)
		if !res.Match {
			res.Margin = math.Inf(-1)
		}
		return res, nil
	}
	margin := math.Inf(1)
	certified := true
	for j := range W {
		if j == predWin {
			continue
		}
		bound := 0.0
		for i := range x {
			bound += math.Abs(W[predWin][i]-W[j][i]) * q.MaxErrBound
		}
		if m := (pred[predWin] - pred[j]) - bound; m < margin {
			margin = m
		}
		if pred[predWin]-pred[j] <= bound {
			certified = false
		}
	}
	res.Margin = margin
	res.Certified = certified
	return res, nil
}
