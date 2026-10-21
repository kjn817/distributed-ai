package progressive

import (
	"errors"
	"math"
	"testing"
)

func TestFloat16RoundTripKnownValues(t *testing.T) {
	cases := []struct {
		in   float32
		want float32
	}{
		{0, 0},
		{1, 1},
		{-2, -2},
		{0.5, 0.5},
		{1.5, 1.5},
		{65504, 65504},               // max finite half
		{1.0009765625, 1.0009765625}, // 1 + 2^-10, representable
		{0.1, 0.0999755859375},       // nearest half to 0.1
		{3.14159265, 3.140625},       // nearest half to pi
	}
	for _, c := range cases {
		got := Float16ToFloat32(Float32ToFloat16(c.in))
		if got != c.want {
			t.Errorf("roundtrip(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if !math.IsInf(float64(Float16ToFloat32(Float32ToFloat16(float32(math.Inf(1))))), 1) {
		t.Error("inf must round-trip")
	}
	if !math.IsNaN(float64(Float16ToFloat32(Float32ToFloat16(float32(math.NaN()))))) {
		t.Error("nan must round-trip")
	}
	// Smallest subnormal half is 2^-24.
	if got := Float16ToFloat32(0x0001); got != float32(math.Ldexp(1, -24)) {
		t.Errorf("min subnormal = %v, want 2^-24", got)
	}
	// Below half of the min subnormal rounds to zero.
	if got := Float32ToFloat16(float32(math.Ldexp(1, -26))); got != 0 {
		t.Errorf("2^-26 -> %#x, want 0", got)
	}
}

func TestQuantizeSymmetricBoundHolds(t *testing.T) {
	x := []float64{-3.7, -0.2, 0, 0.9, 4.2, 1.2345, -4.2}
	for _, bits := range []int{4, 8} {
		q, err := QuantizeSymmetric(x, bits)
		if err != nil {
			t.Fatal(err)
		}
		for i := range x {
			if d := math.Abs(x[i] - q.Dequantized[i]); d > q.MaxErrBound+1e-12 {
				t.Errorf("bits %d elem %d: err %v > bound %v", bits, i, d, q.MaxErrBound)
			}
		}
		// max|x| element must dequantize exactly (it defines the scale).
		if q.Dequantized[4] != 4.2 || q.Dequantized[6] != -4.2 {
			t.Errorf("bits %d: scale-defining element not exact: %v %v", bits, q.Dequantized[4], q.Dequantized[6])
		}
		if q.MaxErrBound != q.Scale/2 {
			t.Errorf("bits %d: bound %v != scale/2 %v", bits, q.MaxErrBound, q.Scale/2)
		}
	}
}

func TestQuantizePayloadBytes(t *testing.T) {
	x := make([]float64, 32)
	for i := range x {
		x[i] = float64(i) / 10
	}
	q4, _ := Quantize(x, 4)
	if q4.PayloadBytes != 16+4 {
		t.Errorf("int4 bytes = %d, want 20 (16 payload + 4 scale)", q4.PayloadBytes)
	}
	q8, _ := Quantize(x, 8)
	if q8.PayloadBytes != 32+4 {
		t.Errorf("int8 bytes = %d, want 36", q8.PayloadBytes)
	}
	q16, _ := Quantize(x, 16)
	if q16.PayloadBytes != 64 {
		t.Errorf("fp16 bytes = %d, want 64 (no scale)", q16.PayloadBytes)
	}
	// Odd length INT4 packs with a padding nibble.
	qOdd, _ := Quantize(make([]float64, 5), 4)
	if qOdd.PayloadBytes != 3+4 {
		t.Errorf("int4 odd bytes = %d, want 7", qOdd.PayloadBytes)
	}
}

func TestQuantizeBoundaries(t *testing.T) {
	// Empty vector: valid, zero bound.
	q, err := Quantize(nil, 8)
	if err != nil || q.MaxErrBound != 0 || len(q.Dequantized) != 0 {
		t.Errorf("empty: %+v err %v", q, err)
	}
	// Zero vector: exact, bound 0.
	q, err = Quantize([]float64{0, 0, 0}, 4)
	if err != nil || q.MaxErrBound != 0 {
		t.Errorf("zero vector: bound %v err %v", q.MaxErrBound, err)
	}
	// Unsupported widths.
	for _, bits := range []int{0, 1, 2, 3, 5, 32, -4} {
		if _, err := Quantize([]float64{1}, bits); !errors.Is(err, ErrUnsupportedBits) {
			t.Errorf("bits %d: err = %v, want ErrUnsupportedBits", bits, err)
		}
	}
	// Non-finite input rejected for integer quantization.
	if _, err := Quantize([]float64{math.NaN()}, 8); err == nil {
		t.Error("NaN input must be rejected")
	}
	if _, err := Quantize([]float64{math.Inf(1)}, 4); err == nil {
		t.Error("Inf input must be rejected")
	}
}

func TestCertifyClearMargin(t *testing.T) {
	W := [][]float64{{1, 0}, {-1, 0}}
	res, err := CertifyAffine(W, nil, []float64{10, 0}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.PredArgmax != 0 || res.RefArgmax != 0 || !res.Match {
		t.Fatalf("unexpected result %+v", res)
	}
	if !res.Certified {
		t.Fatal("clear margin must certify")
	}
	if res.Margin <= 0 {
		t.Fatalf("certified margin must be positive, got %v", res.Margin)
	}
}

func TestCertifyTightMarginNotCertified(t *testing.T) {
	// x = [1, 0.99] quantizes at INT4 to [1, 1] (scale 1/7 rounds 0.99
	// up to 1.0), so the quantized logits tie and the margin is 0 —
	// far below the error bound. Certification must fail even though
	// the prediction still matches the reference: match and certified
	// are different claims.
	W := [][]float64{{1, 0}, {0, 1}}
	res, err := CertifyAffine(W, nil, []float64{1, 0.99}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Match {
		t.Fatalf("prediction should still match here: %+v", res)
	}
	if res.Certified {
		t.Fatal("tied quantized logits must not certify at INT4")
	}
	if res.Margin > 0 {
		t.Fatalf("uncertified margin must be <= 0, got %v", res.Margin)
	}
}

func TestCertifyDimensionErrors(t *testing.T) {
	if _, err := CertifyAffine(nil, nil, []float64{1}, 8); !errors.Is(err, ErrEmptyLayer) {
		t.Errorf("empty W: %v", err)
	}
	if _, err := CertifyAffine([][]float64{{1, 2}}, nil, []float64{1}, 8); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("row mismatch: %v", err)
	}
	if _, err := CertifyAffine([][]float64{{1}}, []float64{1, 2}, []float64{1}, 8); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("bias mismatch: %v", err)
	}
	if _, err := CertifyAffine([][]float64{{1}}, nil, []float64{1}, 7); !errors.Is(err, ErrUnsupportedBits) {
		t.Errorf("bits: %v", err)
	}
}

// The certificate's soundness is the whole point: across many random
// trials, a certified prediction must NEVER disagree with the reference.
// RunExperiment enforces the same invariant internally; here we exercise
// CertifyAffine directly at all precisions.
func TestCertificateSoundnessProperty(t *testing.T) {
	rep, err := RunSynthetic(7, 300, 32, 16)
	if err != nil {
		t.Fatalf("synthetic run (includes soundness check): %v", err)
	}
	for _, r := range rep.Results {
		if r.CertifiedRate > r.ArgmaxMatchRate+1e-12 {
			t.Errorf("bits %d: certified %.3f > matched %.3f — impossible if sound",
				r.Bits, r.CertifiedRate, r.ArgmaxMatchRate)
		}
		if r.ArgmaxMatchRate < 0 || r.ArgmaxMatchRate > 1 || r.CertifiedRate < 0 || r.CertifiedRate > 1 {
			t.Errorf("bits %d: rate out of range: %+v", r.Bits, r)
		}
	}
	// INT8 must compress less and bound tighter than INT4 on this data.
	var r4, r8 *BitsReport
	for i := range rep.Results {
		switch rep.Results[i].Bits {
		case 4:
			r4 = &rep.Results[i]
		case 8:
			r8 = &rep.Results[i]
		}
	}
	if r4 == nil || r8 == nil {
		t.Fatal("missing INT4/INT8 results")
	}
	if !(r4.CompressionRatio > r8.CompressionRatio) {
		t.Errorf("compression: INT4 %.2f should exceed INT8 %.2f", r4.CompressionRatio, r8.CompressionRatio)
	}
	if !(r4.MeanErrBound > r8.MeanErrBound) {
		t.Errorf("err bound: INT4 %.4f should exceed INT8 %.4f", r4.MeanErrBound, r8.MeanErrBound)
	}
}

// Failure cases: the machinery must DETECT bad outcomes, not hide them.
//
//  1. A real argmax mismatch at INT4 (quantization flips the winner)
//     must surface as Match=false, Certified=false.
//  2. An FP16 overflow (finite input saturating half precision) must
//     error out instead of producing a certificate over Inf logits.
//  3. A certified sample that disagreed with the reference would break
//     the soundness invariant — RunExperiment errors on it internally;
//     the property test above sweeps for it across random trials.
func TestCertifyFailureCases(t *testing.T) {
	// (1) x = [0.99, 1.0] quantizes at INT4 to [1, 1]; the tie breaks
	// to row 0 while the reference winner is row 1.
	W := [][]float64{{1, 0}, {0, 1}}
	res, err := CertifyAffine(W, nil, []float64{0.99, 1.0}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Match {
		t.Fatalf("expected a detected mismatch, got %+v", res)
	}
	if res.Certified {
		t.Fatal("a mismatching sample must never be certified")
	}
	// Same input at FP16: represented precisely enough to match and
	// certify — precision changes the outcome, as expected.
	res16, err := CertifyAffine(W, nil, []float64{0.99, 1.0}, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !res16.Match {
		t.Fatalf("FP16 should match here: %+v", res16)
	}

	// (2) FP16 overflow.
	if _, err := CertifyAffine(W, nil, []float64{1e30, 1.0}, 16); err == nil {
		t.Fatal("FP16 overflow (1e30 -> Inf) must error, not certify")
	}
	// Non-finite inputs rejected at all precisions.
	for _, bits := range []int{4, 8, 16} {
		if _, err := CertifyAffine(W, nil, []float64{math.NaN(), 1.0}, bits); err == nil {
			t.Errorf("bits %d: NaN input must error", bits)
		}
		if _, err := CertifyAffine(W, nil, []float64{math.Inf(1), 1.0}, bits); err == nil {
			t.Errorf("bits %d: Inf input must error", bits)
		}
	}
}

func TestActivationSetValidation(t *testing.T) {
	good := &ActivationSet{Kind: KindExactHidden, Source: "test", HiddenSize: 2,
		Vectors: [][]float64{{1, 2}, {3, 4}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	if !good.IsReal() {
		t.Error("exact hidden set must count as real")
	}
	syn := &ActivationSet{Kind: KindSynthetic, Source: "gen", HiddenSize: 1, Vectors: [][]float64{{0}}}
	if syn.IsReal() {
		t.Error("synthetic set must not count as real")
	}
	bad := &ActivationSet{Kind: KindExactHidden, Source: "test", HiddenSize: 3,
		Vectors: [][]float64{{1, 2}}}
	if err := bad.Validate(); err == nil {
		t.Error("width mismatch must fail validation")
	}
	// Round-trip through disk.
	dir := t.TempDir()
	p := dir + "/acts.json"
	if err := SaveActivations(p, good); err != nil {
		t.Fatal(err)
	}
	back, err := LoadActivations(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Kind != good.Kind || len(back.Vectors) != 2 || back.Vectors[1][1] != 4 {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestReportHonestyFields(t *testing.T) {
	rep, err := RunSynthetic(1, 10, 8, 4)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GuaranteeScope != "single_affine_layer_only" {
		t.Errorf("scope = %q", rep.GuaranteeScope)
	}
	if rep.TransformerWideGuarantee {
		t.Error("transformer guarantee must be false")
	}
	if rep.ProgressiveRefinementImplemented {
		t.Error("refinement must be reported as not implemented")
	}
	if rep.ExtraBitsTransmitted != 0 {
		t.Error("no extra bits may be claimed")
	}
	if rep.ExtraRTTCostMs != nil {
		t.Error("extra RTT must be nil (not measured), not a number")
	}
}
