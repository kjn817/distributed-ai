package progressive

// Minimal GGUF reader: just enough of the llama.cpp GGUF container
// format to (a) list tensors with their types and shapes and (b) extract
// tensor values as float64 for the offline experiment.
//
// This is the "real model data" path for the experiment: weights come
// from an actual .gguf file instead of a random generator. It is NOT a
// general GGUF implementation:
//
//   - Supported tensor payload types: F32 (0), F16 (1), Q8_0 (8).
//     Everything else (Q4_0, Q4_K, Q5_*, Q6_K, ...) returns
//     ErrUnsupportedTensorType — honestly, instead of a wrong decode.
//   - Metadata values are skipped generically; only general.alignment
//     and string values the caller asks for are interpreted.
//   - No mmap, no GPU, no llama.cpp interop beyond the file format.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// GGUF tensor type IDs (ggml_type).
const (
	TensorF32  = 0
	TensorF16  = 1
	TensorQ8_0 = 8
)

// TensorTypeName renders a GGUF tensor type ID.
func TensorTypeName(t uint32) string {
	switch t {
	case TensorF32:
		return "F32"
	case TensorF16:
		return "F16"
	case 2:
		return "Q4_0"
	case 3:
		return "Q4_1"
	case 6:
		return "Q5_0"
	case 7:
		return "Q5_1"
	case TensorQ8_0:
		return "Q8_0"
	case 9:
		return "Q8_1"
	case 10:
		return "Q2_K"
	case 11:
		return "Q3_K"
	case 12:
		return "Q4_K"
	case 13:
		return "Q5_K"
	case 14:
		return "Q6_K"
	case 15:
		return "Q8_K"
	default:
		return fmt.Sprintf("type_%d", t)
	}
}

// TensorInfo describes one tensor in a GGUF file. Dims are in GGUF
// storage order (innermost dimension first).
type TensorInfo struct {
	Name     string   `json:"name"`
	Dims     []uint64 `json:"dims"`
	Type     uint32   `json:"type_id"`
	TypeName string   `json:"type"`
	Offset   uint64   `json:"offset"` // relative to the aligned data start
}

// NumElements is the product of the dimensions.
func (t TensorInfo) NumElements() uint64 {
	n := uint64(1)
	for _, d := range t.Dims {
		n *= d
	}
	return n
}

// GGUFFile is a parsed GGUF header: metadata + tensor directory.
type GGUFFile struct {
	Version   uint32            `json:"version"`
	Alignment uint64            `json:"alignment"`
	Tensors   []TensorInfo      `json:"tensors"`
	Strings   map[string]string `json:"-"` // string metadata values
	dataStart int64
}

var (
	ErrNotGGUF                = errors.New("progressive: not a GGUF file")
	ErrUnsupportedGGUFVersion = errors.New("progressive: unsupported GGUF version")
	ErrUnsupportedTensorType  = errors.New("progressive: unsupported tensor type for extraction")
	ErrTensorNotFound         = errors.New("progressive: tensor not found")
)

// ggufReader is a bounds-checked little-endian cursor over an io.ReaderAt.
type ggufReader struct {
	r   io.ReaderAt
	pos int64
	buf [8]byte
}

func (g *ggufReader) read(n int) ([]byte, error) {
	if n < 0 || n > 1<<31 {
		return nil, fmt.Errorf("progressive: unreasonable read size %d", n)
	}
	out := make([]byte, n)
	if _, err := g.r.ReadAt(out, g.pos); err != nil {
		return nil, err
	}
	g.pos += int64(n)
	return out, nil
}

func (g *ggufReader) u32() (uint32, error) {
	b, err := g.read(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (g *ggufReader) u64() (uint64, error) {
	b, err := g.read(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (g *ggufReader) str() (string, error) {
	n, err := g.u64()
	if err != nil {
		return "", err
	}
	if n > 1<<24 {
		return "", fmt.Errorf("progressive: string too long (%d)", n)
	}
	b, err := g.read(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// skipValue advances past one metadata value of the given GGUF type.
// Array element types are handled recursively; strings inside arrays
// carry their own length prefixes.
func (g *ggufReader) skipValue(t uint32, capture func(string)) error {
	switch t {
	case 0, 1: // UINT8, INT8, (BOOL=7 handled below shares 1 byte)
		_, err := g.read(1)
		return err
	case 7: // BOOL
		_, err := g.read(1)
		return err
	case 2, 3: // UINT16, INT16
		_, err := g.read(2)
		return err
	case 4, 5, 6: // UINT32, INT32, FLOAT32
		_, err := g.read(4)
		return err
	case 8: // STRING
		s, err := g.str()
		if err == nil && capture != nil {
			capture(s)
		}
		return err
	case 9: // ARRAY
		et, err := g.u32()
		if err != nil {
			return err
		}
		n, err := g.u64()
		if err != nil {
			return err
		}
		if n > 1<<28 {
			return fmt.Errorf("progressive: array too long (%d)", n)
		}
		for i := uint64(0); i < n; i++ {
			if err := g.skipValue(et, nil); err != nil {
				return err
			}
		}
		return nil
	case 10, 11, 12: // UINT64, INT64, FLOAT64
		_, err := g.read(8)
		return err
	default:
		return fmt.Errorf("progressive: unknown GGUF metadata type %d", t)
	}
}

// ParseGGUF parses the header, metadata and tensor directory of a GGUF
// file. It reads only the header region — tensor payloads are not loaded.
func ParseGGUF(r io.ReaderAt, size int64) (*GGUFFile, error) {
	g := &ggufReader{r: r}
	magic, err := g.read(4)
	if err != nil {
		return nil, err
	}
	if string(magic) != "GGUF" {
		return nil, ErrNotGGUF
	}
	version, err := g.u32()
	if err != nil {
		return nil, err
	}
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedGGUFVersion, version)
	}
	out := &GGUFFile{Version: version, Alignment: 32, Strings: map[string]string{}}
	var tensorCount, kvCount uint64
	if version == 3 {
		if tensorCount, err = g.u64(); err != nil {
			return nil, err
		}
		if kvCount, err = g.u64(); err != nil {
			return nil, err
		}
	} else {
		var tc, kc uint32
		if tc, err = g.u32(); err != nil {
			return nil, err
		}
		if kc, err = g.u32(); err != nil {
			return nil, err
		}
		tensorCount, kvCount = uint64(tc), uint64(kc)
	}
	for i := uint64(0); i < kvCount; i++ {
		key, err := g.str()
		if err != nil {
			return nil, err
		}
		vt, err := g.u32()
		if err != nil {
			return nil, err
		}
		if key == "general.alignment" && vt == 4 {
			v, err := g.u32()
			if err != nil {
				return nil, err
			}
			out.Alignment = uint64(v)
			continue
		}
		var captured string
		isStr := vt == 8
		if err := g.skipValue(vt, func(s string) { captured = s }); err != nil {
			return nil, fmt.Errorf("metadata %q: %w", key, err)
		}
		if isStr {
			out.Strings[key] = captured
		}
	}
	for i := uint64(0); i < tensorCount; i++ {
		name, err := g.str()
		if err != nil {
			return nil, err
		}
		nd, err := g.u32()
		if err != nil {
			return nil, err
		}
		if nd > 4 {
			return nil, fmt.Errorf("progressive: tensor %q has %d dims", name, nd)
		}
		ti := TensorInfo{Name: name}
		for d := uint32(0); d < nd; d++ {
			var dim uint64
			if version == 3 {
				dim, err = g.u64()
			} else {
				var d32 uint32
				d32, err = g.u32()
				dim = uint64(d32)
			}
			if err != nil {
				return nil, err
			}
			ti.Dims = append(ti.Dims, dim)
		}
		tt, err := g.u32()
		if err != nil {
			return nil, err
		}
		ti.Type = tt
		ti.TypeName = TensorTypeName(tt)
		off, err := g.u64()
		if err != nil {
			return nil, err
		}
		ti.Offset = off
		out.Tensors = append(out.Tensors, ti)
	}
	// Tensor data starts at the next alignment boundary after the header.
	align := int64(out.Alignment)
	if align <= 0 {
		align = 32
	}
	out.dataStart = (g.pos + align - 1) / align * align
	if out.dataStart > size {
		return nil, fmt.Errorf("progressive: header ends past end of file")
	}
	return out, nil
}

// Find returns the tensor directory entry for name.
func (f *GGUFFile) Find(name string) (TensorInfo, bool) {
	for _, t := range f.Tensors {
		if t.Name == name {
			return t, true
		}
	}
	return TensorInfo{}, false
}

// ExtractTensor reads and dequantizes one tensor's values.
// Supported payload types: F32, F16, Q8_0. maxElems > 0 caps how many
// elements are returned (the first ones) so callers can sample a large
// tensor without loading it whole; the returned count is the number of
// elements actually returned.
func (f *GGUFFile) ExtractTensor(r io.ReaderAt, size int64, name string, maxElems int) ([]float64, error) {
	ti, ok := f.Find(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTensorNotFound, name)
	}
	n := ti.NumElements()
	want := n
	if maxElems > 0 && uint64(maxElems) < want {
		want = uint64(maxElems)
	}
	start := f.dataStart + int64(ti.Offset)
	switch ti.Type {
	case TensorF32:
		raw := make([]byte, want*4)
		if _, err := r.ReadAt(raw, start); err != nil {
			return nil, err
		}
		out := make([]float64, want)
		for i := range out {
			out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:])))
		}
		return out, nil
	case TensorF16:
		raw := make([]byte, want*2)
		if _, err := r.ReadAt(raw, start); err != nil {
			return nil, err
		}
		out := make([]float64, want)
		for i := range out {
			out[i] = float64(Float16ToFloat32(binary.LittleEndian.Uint16(raw[i*2:])))
		}
		return out, nil
	case TensorQ8_0:
		// Blocks of 32 values: FP16 scale + 32 int8 codes (34 bytes).
		const blockVals, blockBytes = 32, 34
		blocks := (want + blockVals - 1) / blockVals
		raw := make([]byte, blocks*blockBytes)
		if _, err := r.ReadAt(raw, start); err != nil {
			return nil, err
		}
		out := make([]float64, 0, want)
		for b := uint64(0); b < blocks && uint64(len(out)) < want; b++ {
			blk := raw[b*blockBytes:]
			scale := float64(Float16ToFloat32(binary.LittleEndian.Uint16(blk)))
			for i := 0; i < blockVals && uint64(len(out)) < want; i++ {
				out = append(out, float64(int8(blk[2+i]))*scale)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: tensor %s is %s", ErrUnsupportedTensorType, name,
			ti.TypeName+" — only F32, F16 and Q8_0 payloads can be extracted")
	}
}

// TensorNames is a convenience for error messages and CLI listing.
func (f *GGUFFile) TensorNames() []string {
	out := make([]string, len(f.Tensors))
	for i, t := range f.Tensors {
		out[i] = t.Name
	}
	return out
}
