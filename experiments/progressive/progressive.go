// Package progressive is an OFFLINE experiment: certified activation
// quantization for ONE affine layer z = W·x + b.
//
// Scope — read this before believing any number from this package:
//
//   - The certificate proves: for this one affine map, if the quantized
//     input's winner beats every competitor by more than the worst-case
//     pairwise error bound, the FP32 reference argmax cannot change.
//   - The certificate does NOT cover a Transformer. Error propagation
//     through attention, norms and stacked layers is unimplemented and
//     unproven here; GuaranteeScope says so in every report.
//   - INT4 and INT8 here are independent quantizations of the same
//     vector. A progressive bitplane refinement stream (send 4 bits,
//     then 4 more to reach 8) is NOT implemented: no extra bits are
//     transmitted by any code in this package, and reports state
//     ProgressiveRefinementImplemented=false with a nil ExtraRTTCostMs
//     instead of a success claim.
//   - FP16 means per-element IEEE 754 half-precision rounding of the
//     activation vector (a 16-bit baseline), not a quantization scheme.
//
// The package has no network, service, or ledger dependencies and is not
// imported by the production service.
package progressive

import (
	"errors"
	"fmt"
	"math"
)

// Quantized is the result of quantizing one activation vector.
type Quantized struct {
	Bits int
	// Dequantized is the reconstructed vector (same length as the input).
	Dequantized []float64
	// Scale is the symmetric quantization step (0 for FP16).
	Scale float64
	// MaxErrBound is a sound a-priori bound: |x_i - Dequantized_i| <=
	// MaxErrBound for every i. Symmetric: scale/2. FP16: max half-ULP/2
	// over the elements. Certification uses this bound, never the
	// observed (smaller) error.
	MaxErrBound float64
	// PayloadBytes is the wire size of the quantized form:
	// ceil(n*bits/8) plus 4 bytes for the FP32 scale (INT4/INT8 only).
	PayloadBytes int
}

// ErrUnsupportedBits is returned for bit widths other than 4, 8, 16.
var ErrUnsupportedBits = errors.New("progressive: bits must be 4, 8 or 16")

// Quantize dispatches on bits: 4/8 symmetric integer quantization,
// 16 per-element FP16 rounding.
func Quantize(x []float64, bits int) (Quantized, error) {
	switch bits {
	case 4, 8:
		return QuantizeSymmetric(x, bits)
	case 16:
		return QuantizeFP16(x), nil
	default:
		return Quantized{}, ErrUnsupportedBits
	}
}

// QuantizeSymmetric applies per-tensor symmetric quantization:
// scale = max|x| / qmax, q = clamp(round(x/scale)), dq = q*scale.
// The error bound scale/2 holds for every element by construction
// (round-to-nearest), including clipped ones since |x| <= max|x|.
func QuantizeSymmetric(x []float64, bits int) (Quantized, error) {
	if bits != 4 && bits != 8 {
		return Quantized{}, ErrUnsupportedBits
	}
	q := Quantized{Bits: bits, Dequantized: make([]float64, len(x))}
	if len(x) == 0 {
		return q, nil
	}
	qmaxInt := (1 << (bits - 1)) - 1
	qmax := float64(qmaxInt)
	maxAbs := 0.0
	for _, v := range x {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Quantized{}, fmt.Errorf("progressive: non-finite activation %v", v)
		}
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		// Zero vector: exact at any precision, bound 0.
		q.PayloadBytes = payloadBytes(len(x), bits)
		return q, nil
	}
	scale := maxAbs / qmax
	q.Scale = scale
	q.MaxErrBound = scale / 2
	for i, v := range x {
		n := math.Round(v / scale)
		if n > qmax {
			n = qmax
		} else if n < -qmax {
			n = -qmax
		}
		q.Dequantized[i] = n * scale
	}
	q.PayloadBytes = payloadBytes(len(x), bits)
	return q, nil
}

// payloadBytes: packed codes + one FP32 scale.
func payloadBytes(n, bits int) int {
	return (n*bits+7)/8 + 4
}

// QuantizeFP16 rounds each element through IEEE 754 half precision.
// MaxErrBound is the maximum half-ULP/2 over the dequantized elements,
// which bounds each element's rounding error.
func QuantizeFP16(x []float64) Quantized {
	q := Quantized{Bits: 16, Dequantized: make([]float64, len(x)), PayloadBytes: len(x) * 2}
	for i, v := range x {
		dq := float64(Float16ToFloat32(Float32ToFloat16(float32(v))))
		q.Dequantized[i] = dq
		if b := halfErrBound(dq); b > q.MaxErrBound {
			q.MaxErrBound = b
		}
	}
	return q
}

// halfErrBound returns half the ULP of a finite FP16-representable value
// (0 for zero; for subnormals the ULP is the minimum subnormal step).
func halfErrBound(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	// exponent of |v| in [2^e, 2^(e+1))
	_, e := math.Frexp(math.Abs(v)) // v = frac * 2^e, frac in [0.5,1)
	// FP16 has 10 mantissa bits; values below 2^-14 are subnormal with a
	// fixed step of 2^-24.
	exp := e - 1
	if exp < -14 {
		return math.Ldexp(1, -25) // (2^-24)/2
	}
	return math.Ldexp(1, exp-11) // ULP = 2^(exp-10); half of it
}

// Float32ToFloat16 converts a float32 to IEEE 754 half-precision bits
// (round-to-nearest-even, with subnormal, Inf and NaN handling).
func Float32ToFloat16(f float32) uint16 {
	bits := math.Float32bits(f)
	sign := uint16((bits >> 16) & 0x8000)
	exp := int((bits>>23)&0xff) - 127 + 15
	mant := bits & 0x7fffff
	switch {
	case exp >= 31: // overflow / Inf / NaN
		if (bits>>23)&0xff == 0xff {
			if mant != 0 {
				return sign | 0x7e00 // NaN
			}
			return sign | 0x7c00 // Inf
		}
		return sign | 0x7c00 // saturate to Inf
	case exp <= 0: // subnormal or zero in half
		if exp < -10 {
			return sign // underflows to zero
		}
		mant |= 0x800000
		shift := uint(14 - exp)
		// round to nearest even
		half := mant >> shift
		rem := mant & ((1 << shift) - 1)
		mid := uint32(1) << (shift - 1)
		if rem > mid || (rem == mid && half&1 == 1) {
			half++
		}
		return sign | uint16(half)
	default:
		half := uint32(exp)<<10 | (mant >> 13)
		rem := mant & 0x1fff
		if rem > 0x1000 || (rem == 0x1000 && half&1 == 1) {
			half++
		}
		return sign | uint16(half)
	}
}

// Float16ToFloat32 converts IEEE 754 half-precision bits to a float32.
func Float16ToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := (h >> 10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch {
	case exp == 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal: normalize
		e := int32(-14)
		m := mant
		for m&0x400 == 0 {
			m <<= 1
			e--
		}
		m &= 0x3ff
		return math.Float32frombits(sign | uint32(e+127)<<23 | m<<13)
	case exp == 31:
		if mant == 0 {
			return math.Float32frombits(sign | 0x7f800000) // Inf
		}
		return math.Float32frombits(sign | 0x7f800000 | mant<<13 | 0x400000) // NaN
	default:
		return math.Float32frombits(sign | uint32(int(exp)-15+127)<<23 | mant<<13)
	}
}
