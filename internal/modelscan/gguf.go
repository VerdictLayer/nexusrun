// Package modelscan finds the model files on this machine and says what each
// one is and whether this machine can run it.
//
// It reads only the header of a GGUF file, never the weights, and treats every
// file as hostile: a model folder can hold anything, so every length in the
// header is bounded before it is believed.
package modelscan

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// GGUF value types.
const (
	tUint8 = iota
	tInt8
	tUint16
	tInt16
	tUint32
	tInt32
	tFloat32
	tBool
	tString
	tArray
	tUint64
	tInt64
	tFloat64
)

const (
	maxHeaderBytes = 256 << 20 // tokenizer vocabularies make headers large, not unbounded
	maxKeyBytes    = 1 << 10
	maxStringBytes = 16 << 20
	maxKV          = 1 << 16
	maxArrayLen    = 1 << 26
)

// Header is what a GGUF file says about itself.
type Header struct {
	Version      uint32
	Architecture string
	Name         string
	SizeLabel    string // e.g. "7B", when the file records one
	FileType     uint32
	HasFileType  bool
	HasChat      bool // carries a chat template
	Context      uint64
	Layers       uint64
	Embedding    uint64
	Heads        uint64
	KVHeads      uint64
	Experts      uint64
}

var errNotGGUF = errors.New("not a GGUF file")

// ReadHeader parses the metadata of the GGUF file at path.
func ReadHeader(path string) (*Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(bufio.NewReaderSize(io.LimitReader(f, maxHeaderBytes), 1<<16))
}

type reader struct{ r *bufio.Reader }

func (r reader) u32() (uint32, error) {
	var b [4]byte
	_, err := io.ReadFull(r.r, b[:])
	return binary.LittleEndian.Uint32(b[:]), err
}

func (r reader) u64() (uint64, error) {
	var b [8]byte
	_, err := io.ReadFull(r.r, b[:])
	return binary.LittleEndian.Uint64(b[:]), err
}

func (r reader) skip(n uint64) error {
	// A length past the header limit can only be corrupt, and above 2^63 it
	// would wrap negative in CopyN and be "skipped" as nothing at all.
	if n > maxHeaderBytes {
		return fmt.Errorf("a field of %d bytes exceeds the header limit", n)
	}
	_, err := io.CopyN(io.Discard, r.r, int64(n))
	return err
}

func (r reader) str(limit uint64) (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("string of %d bytes exceeds the limit", n)
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r.r, b)
	return string(b), err
}

// number reads a numeric value of type t, or reports that it is not one.
func (r reader) number(t uint32) (uint64, bool, error) {
	switch t {
	case tUint8, tInt8, tBool:
		b, err := r.r.ReadByte()
		return uint64(b), true, err
	case tUint16, tInt16:
		var b [2]byte
		_, err := io.ReadFull(r.r, b[:])
		return uint64(binary.LittleEndian.Uint16(b[:])), true, err
	case tUint32, tInt32:
		v, err := r.u32()
		return uint64(v), true, err
	case tUint64, tInt64:
		v, err := r.u64()
		return v, true, err
	}
	return 0, false, nil
}

var fixedSize = map[uint32]uint64{tUint8: 1, tInt8: 1, tBool: 1, tUint16: 2, tInt16: 2, tUint32: 4, tInt32: 4, tFloat32: 4, tUint64: 8, tInt64: 8, tFloat64: 8}

// skipValue discards a value of type t, bounded in length and nesting.
func (r reader) skipValue(t uint32, depth int) error {
	if n, ok := fixedSize[t]; ok {
		return r.skip(n)
	}
	switch t {
	case tString:
		n, err := r.u64()
		if err != nil {
			return err
		}
		return r.skip(n)
	case tArray:
		if depth > 1 {
			return errors.New("arrays nested too deeply")
		}
		et, err := r.u32()
		if err != nil {
			return err
		}
		n, err := r.u64()
		if err != nil {
			return err
		}
		if n > maxArrayLen {
			return fmt.Errorf("array of %d elements exceeds the limit", n)
		}
		if size, ok := fixedSize[et]; ok {
			return r.skip(n * size)
		}
		for i := uint64(0); i < n; i++ {
			if err := r.skipValue(et, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown value type %d", t)
}

func parse(br *bufio.Reader) (*Header, error) {
	r := reader{br}
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return nil, errNotGGUF
	}
	h := &Header{}
	var err error
	if h.Version, err = r.u32(); err != nil {
		return nil, err
	}
	if h.Version < 2 || h.Version > 3 {
		return nil, fmt.Errorf("unsupported GGUF version %d", h.Version)
	}
	if _, err = r.u64(); err != nil { // tensor count
		return nil, err
	}
	kvs, err := r.u64()
	if err != nil {
		return nil, err
	}
	if kvs > maxKV {
		return nil, fmt.Errorf("header declares %d entries", kvs)
	}

	nums := map[string]uint64{}
	for i := uint64(0); i < kvs; i++ {
		key, err := r.str(maxKeyBytes)
		if err != nil {
			return nil, err
		}
		t, err := r.u32()
		if err != nil {
			return nil, err
		}
		if t == tString {
			switch key {
			case "general.architecture", "general.name", "general.size_label":
				s, err := r.str(maxStringBytes)
				if err != nil {
					return nil, err
				}
				switch key {
				case "general.architecture":
					h.Architecture = s
				case "general.name":
					h.Name = s
				default:
					h.SizeLabel = s
				}
				continue
			case "tokenizer.chat_template":
				h.HasChat = true
			}
			if err := r.skipValue(t, 0); err != nil {
				return nil, err
			}
			continue
		}
		if v, ok, err := r.number(t); err != nil {
			return nil, err
		} else if ok {
			if key == "general.file_type" {
				h.FileType, h.HasFileType = uint32(v), true
			}
			for _, suffix := range numericSuffixes {
				if strings.HasSuffix(key, suffix) {
					nums[key] = v
				}
			}
			continue
		}
		if err := r.skipValue(t, 0); err != nil {
			return nil, err
		}
	}

	if h.Architecture == "" {
		return nil, errors.New("file does not name an architecture")
	}
	a := h.Architecture + "."
	h.Context = nums[a+"context_length"]
	h.Layers = nums[a+"block_count"]
	h.Embedding = nums[a+"embedding_length"]
	h.Heads = nums[a+"attention.head_count"]
	h.KVHeads = nums[a+"attention.head_count_kv"]
	h.Experts = nums[a+"expert_count"]
	return h, nil
}

var numericSuffixes = []string{".context_length", ".block_count", ".embedding_length", ".attention.head_count", ".attention.head_count_kv", ".expert_count"}

// quantNames maps llama.cpp's file-type enum to its conventional name.
var quantNames = map[uint32]string{
	0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
	10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L", 14: "Q4_K_S", 15: "Q4_K_M",
	16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K", 19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S",
	22: "IQ3_XS", 23: "IQ3_XXS", 24: "IQ1_S", 25: "IQ4_NL", 26: "IQ3_S", 27: "IQ3_M",
	28: "IQ2_S", 29: "IQ2_M", 30: "IQ4_XS", 31: "IQ1_M", 32: "BF16",
}

// Quantization names the file type, or "" when the file does not say.
func (h *Header) Quantization() string {
	if !h.HasFileType {
		return ""
	}
	if n, ok := quantNames[h.FileType]; ok {
		return n
	}
	return fmt.Sprintf("type %d", h.FileType)
}
