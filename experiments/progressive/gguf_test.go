package progressive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// buildFixtureGGUF writes a minimal valid GGUF v3 file: one string
// metadata entry, one alignment entry, an F32 tensor and a Q8_0 tensor.
func buildFixtureGGUF(t *testing.T) ([]byte, []float64, []float64) {
	t.Helper()
	var buf bytes.Buffer
	w := func(v any) {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}
	ws := func(s string) {
		w(uint64(len(s)))
		buf.WriteString(s)
	}
	buf.WriteString("GGUF")
	w(uint32(3)) // version
	w(uint64(2)) // tensor count
	w(uint64(2)) // kv count
	ws("general.name")
	w(uint32(8)) // STRING
	ws("fixture")
	ws("general.alignment")
	w(uint32(4)) // UINT32
	w(uint32(32))

	f32vals := []float64{1.5, -2.25, 3.0, 0.125, -0.5, 7.75} // 2 rows x 3
	q8vals := make([]float64, 32)
	for i := range q8vals {
		q8vals[i] = float64(i-16) * 0.5 // exactly representable with scale 0.5
	}
	// Tensor infos: name, ndims, dims (innermost first), type, offset.
	ws("w_f32")
	w(uint32(2))
	w(uint64(3))
	w(uint64(2))
	w(uint32(TensorF32))
	w(uint64(0))
	q8offset := uint64(len(f32vals) * 4)
	ws("w_q8")
	w(uint32(1))
	w(uint64(32))
	w(uint32(TensorQ8_0))
	w(q8offset)

	// Align to 32.
	for buf.Len()%32 != 0 {
		buf.WriteByte(0)
	}
	for _, v := range f32vals {
		w(math.Float32bits(float32(v)))
	}
	// Q8_0 block: fp16 scale 0.5, codes = value/0.5 = i-16.
	w(Float32ToFloat16(0.5))
	for i := 0; i < 32; i++ {
		buf.WriteByte(byte(int8(i - 16)))
	}
	return buf.Bytes(), f32vals, q8vals
}

func TestParseAndExtractFixture(t *testing.T) {
	data, f32want, q8want := buildFixtureGGUF(t)
	r := bytes.NewReader(data)
	gf, err := ParseGGUF(r, int64(len(data)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gf.Version != 3 || len(gf.Tensors) != 2 {
		t.Fatalf("header: version %d tensors %d", gf.Version, len(gf.Tensors))
	}
	if gf.Strings["general.name"] != "fixture" {
		t.Errorf("string metadata = %q", gf.Strings["general.name"])
	}
	ti, ok := gf.Find("w_f32")
	if !ok || ti.TypeName != "F32" || len(ti.Dims) != 2 || ti.Dims[0] != 3 || ti.Dims[1] != 2 {
		t.Fatalf("w_f32 info: %+v ok=%v", ti, ok)
	}
	got, err := gf.ExtractTensor(r, int64(len(data)), "w_f32", 0)
	if err != nil {
		t.Fatalf("extract f32: %v", err)
	}
	if len(got) != len(f32want) {
		t.Fatalf("f32 values: %d want %d", len(got), len(f32want))
	}
	for i := range got {
		if got[i] != f32want[i] {
			t.Errorf("f32[%d] = %v want %v", i, got[i], f32want[i])
		}
	}
	got8, err := gf.ExtractTensor(r, int64(len(data)), "w_q8", 0)
	if err != nil {
		t.Fatalf("extract q8_0: %v", err)
	}
	for i := range got8 {
		if got8[i] != q8want[i] {
			t.Errorf("q8[%d] = %v want %v", i, got8[i], q8want[i])
		}
	}
	// maxElems caps the extraction.
	part, err := gf.ExtractTensor(r, int64(len(data)), "w_f32", 2)
	if err != nil || len(part) != 2 {
		t.Errorf("capped extract: %v len %d", err, len(part))
	}
	// WeightsFromTensor reshapes row-major [in=3, out=2].
	W, err := WeightsFromTensor(got, ti.Dims, 0)
	if err != nil || len(W) != 2 || len(W[0]) != 3 || W[1][2] != 7.75 {
		t.Errorf("reshape: W=%v err=%v", W, err)
	}
}

func TestGGUFErrors(t *testing.T) {
	if _, err := ParseGGUF(bytes.NewReader([]byte("NOPE....")), 8); !errors.Is(err, ErrNotGGUF) {
		t.Errorf("bad magic: %v", err)
	}
	data, _, _ := buildFixtureGGUF(t)
	r := bytes.NewReader(data)
	gf, err := ParseGGUF(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gf.ExtractTensor(r, int64(len(data)), "nope", 0); !errors.Is(err, ErrTensorNotFound) {
		t.Errorf("missing tensor: %v", err)
	}
	// Truncated file must error, not panic or fabricate values.
	trunc := bytes.NewReader(data[:len(data)-10])
	gf2, err := ParseGGUF(trunc, int64(len(data)-10))
	if err != nil {
		t.Skipf("truncated header rejected at parse time (%v) — also acceptable", err)
	}
	if _, err := gf2.ExtractTensor(trunc, int64(len(data)-10), "w_q8", 0); err == nil {
		t.Error("extract from truncated file must fail")
	}
}

// TestRealModelGGUF exercises the extraction path against the bundled
// Qwen2.5-0.5B GGUF when it is present (it is not part of the repo, so
// CI without the file skips). Parsing must succeed; extraction is
// attempted on the first supported-type tensor.
func TestRealModelGGUF(t *testing.T) {
	path := filepath.Join("..", "..", "models", "qwen2.5-0.5b-instruct-q4_k_m.gguf")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("bundled model not present: %v", err)
	}
	defer f.Close()
	st, _ := f.Stat()
	gf, err := ParseGGUF(f, st.Size())
	if err != nil {
		t.Fatalf("parse real GGUF: %v", err)
	}
	if len(gf.Tensors) == 0 {
		t.Fatal("real GGUF has no tensors")
	}
	t.Logf("real GGUF: version %d, %d tensors, name=%q", gf.Version, len(gf.Tensors), gf.Strings["general.name"])
	extracted := ""
	for _, ti := range gf.Tensors {
		if ti.Type != TensorF32 && ti.Type != TensorF16 && ti.Type != TensorQ8_0 {
			continue
		}
		vals, err := gf.ExtractTensor(f, st.Size(), ti.Name, 64)
		if err != nil {
			t.Fatalf("extract %s (%s): %v", ti.Name, ti.TypeName, err)
		}
		if len(vals) == 0 {
			t.Fatalf("extract %s returned no values", ti.Name)
		}
		for _, v := range vals {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("extract %s produced non-finite value", ti.Name)
			}
		}
		extracted = ti.Name + " (" + ti.TypeName + ")"
		break
	}
	if extracted == "" {
		t.Skip("real GGUF has no F32/F16/Q8_0 tensor to extract (all K-quant) — parser verified, extraction path covered by fixture")
	}
	t.Logf("extracted real tensor values from %s", extracted)
	// And an unsupported type must be refused honestly, if one exists.
	for _, ti := range gf.Tensors {
		if ti.Type != TensorF32 && ti.Type != TensorF16 && ti.Type != TensorQ8_0 {
			if _, err := gf.ExtractTensor(f, st.Size(), ti.Name, 4); !errors.Is(err, ErrUnsupportedTensorType) {
				t.Errorf("unsupported tensor %s (%s): err = %v", ti.Name, ti.TypeName, err)
			}
			break
		}
	}
}
