package modelscan

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/hardware"
)

// gguf builds a GGUF header the way llama.cpp writes one.
type gguf struct{ buf bytes.Buffer }

func (g *gguf) u32(v uint32) { binary.Write(&g.buf, binary.LittleEndian, v) }
func (g *gguf) u64(v uint64) { binary.Write(&g.buf, binary.LittleEndian, v) }
func (g *gguf) str(s string) { g.u64(uint64(len(s))); g.buf.WriteString(s) }

func build(kvs func(g *gguf) int) []byte {
	body := &gguf{}
	n := kvs(body)
	out := &gguf{}
	out.buf.WriteString("GGUF")
	out.u32(3)
	out.u64(0)
	out.u64(uint64(n))
	out.buf.Write(body.buf.Bytes())
	return out.buf.Bytes()
}

func (g *gguf) kvStr(k, v string) { g.str(k); g.u32(tString); g.str(v) }
func (g *gguf) kvU32(k string, v uint32) {
	g.str(k)
	g.u32(tUint32)
	g.u32(v)
}
func (g *gguf) kvU64(k string, v uint64) {
	g.str(k)
	g.u32(tUint64)
	g.u64(v)
}

// llama is a small chat model: 8 layers, 8 heads, 2 KV heads, 512 wide.
func llama(g *gguf) int {
	g.kvStr("general.architecture", "llama")
	g.kvStr("general.name", "Tiny Chat")
	g.kvStr("general.size_label", "0.5B")
	g.kvU32("general.file_type", 15)
	g.kvU32("llama.context_length", 8192)
	g.kvU32("llama.block_count", 8)
	g.kvU32("llama.embedding_length", 512)
	g.kvU32("llama.attention.head_count", 8)
	g.kvU32("llama.attention.head_count_kv", 2)
	g.kvStr("tokenizer.chat_template", "{{ messages }}")
	// A vocabulary: an array of strings, which the reader must skip.
	g.str("tokenizer.ggml.tokens")
	g.u32(tArray)
	g.u32(tString)
	g.u64(3)
	for _, t := range []string{"a", "bb", "ccc"} {
		g.str(t)
	}
	return 11
}

func write(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadHeaderExtractsWhatMatters(t *testing.T) {
	p := write(t, t.TempDir(), "m.gguf", build(llama))
	h, err := ReadHeader(p)
	if err != nil {
		t.Fatal(err)
	}
	if h.Architecture != "llama" || h.Name != "Tiny Chat" || h.SizeLabel != "0.5B" {
		t.Errorf("identity wrong: %+v", h)
	}
	if h.Quantization() != "Q4_K_M" || !h.HasChat || h.Context != 8192 || h.Layers != 8 || h.KVHeads != 2 {
		t.Errorf("details wrong: %+v quant=%q", h, h.Quantization())
	}
}

func TestReadHeaderRefusesHostileFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"not gguf":    []byte("this is not a model at all"),
		"empty":       nil,
		"truncated":   build(llama)[:40],
		"old version": append([]byte("GGUF\x01\x00\x00\x00"), make([]byte, 32)...),
		"huge kv count": func() []byte {
			g := &gguf{}
			g.buf.WriteString("GGUF")
			g.u32(3)
			g.u64(0)
			g.u64(1 << 40)
			return g.buf.Bytes()
		}(),
		"giant string": build(func(g *gguf) int {
			g.str("general.name")
			g.u32(tString)
			g.u64(1 << 62) // would wrap negative if cast carelessly
			return 1
		}),
		"giant array": build(func(g *gguf) int {
			g.str("tokenizer.ggml.tokens")
			g.u32(tArray)
			g.u32(tString)
			g.u64(1 << 60)
			return 1
		}),
		"no architecture": build(func(g *gguf) int { g.kvStr("general.name", "x"); return 1 }),
	}
	for name, data := range cases {
		p := write(t, dir, strings.ReplaceAll(name, " ", "_")+".gguf", data)
		if h, err := ReadHeader(p); err == nil {
			t.Errorf("%s: want an error, got %+v", name, h)
		}
	}
}

func machine(ramMB, vramMB int64) *hardware.Report {
	r := &hardware.Report{TotalRAMMB: ramMB, Devices: []hardware.Device{{Class: hardware.ClassCPU}}}
	if vramMB > 0 {
		r.Devices = append(r.Devices, hardware.Device{Class: hardware.ClassGPU, Vendor: "nvidia", MemoryMB: vramMB})
	}
	return r
}

func TestEstimateJudgesEachDevice(t *testing.T) {
	h := &Header{Layers: 8, Heads: 8, Embedding: 512, KVHeads: 2, Context: 8192}
	// 300 MB of weights + 8 layers * 4096 ctx * 2 kv heads * 64 dim * 2 B * 2 (K,V) = 16 MB cache + 512 overhead
	need := NeedMB(h, 300<<20, 4096)
	if need != 300+16+512 {
		t.Fatalf("need = %d MB, want 828", need)
	}
	byDev := map[string]string{}
	for _, f := range Estimate(h, 300<<20, 4096, machine(1000, 600)) {
		byDev[f.Device] = f.Verdict
	}
	if byDev["cpu"] != "tight" || byDev["gpu"] != "too_large" {
		t.Errorf("verdicts = %v", byDev) // 828 vs 1000 RAM is tight; vs 600 VRAM does not fit
	}
	if v := Estimate(h, 300<<20, 4096, machine(16000, 0))[0].Verdict; v != "fits" {
		t.Errorf("plenty of RAM should fit, got %s", v)
	}
	if v := Estimate(h, 300<<20, 4096, machine(0, 0))[0].Verdict; v != "unknown" {
		t.Errorf("unknown memory must not be guessed, got %s", v)
	}
}

func TestScanFolderFindsModelsSkipsShardsAndProjectors(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "chat/tiny.gguf", build(llama))
	write(t, dir, "split/big-00001-of-00002.gguf", build(llama))
	write(t, dir, "split/big-00002-of-00002.gguf", []byte("second shard has no header"))
	write(t, dir, "vision/mmproj.gguf", build(func(g *gguf) int { g.kvStr("general.architecture", "clip"); return 1 }))
	write(t, dir, "broken/bad.gguf", []byte("garbage"))
	write(t, dir, "notes.txt", []byte("ignored"))

	ms := Scan(Options{Dirs: []string{dir}, Hardware: machine(16000, 0), SkipOllama: true})
	byName := map[string]Model{}
	for _, m := range ms {
		byName[filepath.Base(m.Path)] = m
	}
	if _, ok := byName["mmproj.gguf"]; ok {
		t.Error("a vision projector is not a model")
	}
	if _, ok := byName["big-00002-of-00002.gguf"]; ok {
		t.Error("only the first shard should be listed")
	}
	tiny := byName["tiny.gguf"]
	if tiny.Ref != tiny.Path || tiny.Origin != OriginFolder || !tiny.Chat || tiny.Quantization != "Q4_K_M" || len(tiny.Fit) == 0 {
		t.Errorf("tiny = %+v", tiny)
	}
	if bad := byName["bad.gguf"]; bad.Error == "" {
		t.Error("an unreadable file must be reported, not hidden")
	}
	if ms[len(ms)-1].Error == "" {
		t.Error("errors sort last")
	}
}

func TestScanFollowsSymlinkedFilesButNotFolders(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs")
	target := write(t, blobs, "abc123", build(llama))
	snap := filepath.Join(dir, "snapshots", "rev")
	os.MkdirAll(snap, 0o755)
	if err := os.Symlink(target, filepath.Join(snap, "model.gguf")); err != nil {
		t.Skip("symlinks unavailable")
	}
	outside := t.TempDir()
	write(t, outside, "secret.gguf", build(llama))
	os.Symlink(outside, filepath.Join(dir, "linked-folder"))

	ms := Scan(Options{Dirs: []string{dir}, Hardware: machine(16000, 0), SkipOllama: true})
	if len(ms) != 1 {
		t.Fatalf("want the one symlinked file, once; got %d: %+v", len(ms), ms)
	}
}

func TestScanReadsOllamaModelsWithoutCopyingThem(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OLLAMA_MODELS", root)
	blob := write(t, root, "blobs/sha256-"+strings.Repeat("a", 64), build(llama))
	_ = blob
	manifest := `{"config":{"digest":"sha256:` + strings.Repeat("b", 64) + `"},"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:` + strings.Repeat("a", 64) + `","size":511}]}`
	write(t, root, "manifests/registry.ollama.ai/library/tinyscan/latest", []byte(manifest))

	var got *Model
	for _, m := range Scan(Options{Hardware: machine(16000, 0)}) {
		if m.Ref == "ollama:tinyscan:latest" {
			m := m
			got = &m
		}
	}
	if got == nil {
		t.Fatal("the Ollama model was not found")
	}
	if got.Origin != OriginOllama || got.Architecture != "llama" || !got.Chat || got.Parameters != "0.5B" {
		t.Errorf("model = %+v", got)
	}
	if !strings.HasPrefix(got.Path, root) {
		t.Errorf("must point at Ollama's own blob, got %s", got.Path)
	}
}
