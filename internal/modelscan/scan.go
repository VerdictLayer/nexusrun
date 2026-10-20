package modelscan

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/store"
)

// Origin says where a model was found.
const (
	OriginOllama = "ollama"
	OriginStore  = "store"
	OriginFolder = "folder"
)

// DefaultContext is the context window fit is estimated for, matching what
// NexusRun's tested configurations use.
const DefaultContext = 4096

const (
	maxFolderFiles = 2000
	maxFolderDepth = 6
)

// Fit is whether one device on this machine can hold a model.
type Fit struct {
	Device   string `json:"device"` // gpu | cpu
	MemoryMB int64  `json:"memory_mb,omitempty"`
	NeedMB   int64  `json:"need_mb"`
	// Verdict is fits (comfortable), tight (may run, little headroom),
	// too_large, or unknown (the device's memory is not known).
	Verdict string `json:"verdict"`
}

// Model is one model file found locally.
type Model struct {
	// Ref is what goes in `model:` or `source:` to use this model.
	Ref          string `json:"ref"`
	Origin       string `json:"origin"`
	Name         string `json:"name,omitempty"`
	Path         string `json:"path"`
	SizeBytes    int64  `json:"size_bytes"`
	Architecture string `json:"architecture,omitempty"`
	Parameters   string `json:"parameters,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	ContextLen   uint64 `json:"context_length,omitempty"`
	Chat         bool   `json:"chat"`
	MoE          bool   `json:"moe,omitempty"`
	Fit          []Fit  `json:"fit,omitempty"`
	// Error is set when the file could not be read as a model; Ref and Path
	// still say which file.
	Error string `json:"error,omitempty"`
}

// Options controls a scan.
type Options struct {
	Store *store.Store
	// Dirs are extra folders to search for .gguf files. Nothing outside Ollama's
	// own folders, the NexusRun store and these is ever read.
	Dirs []string
	// Context is the window to estimate memory for; zero means DefaultContext.
	Context int
	// Hardware is the machine to judge fit against; nil detects it.
	Hardware *hardware.Report
	// SkipOllama leaves Ollama's folders out of the scan.
	SkipOllama bool
}

// Scan lists the local models, most capable first.
func Scan(o Options) []Model {
	ctxLen := o.Context
	if ctxLen <= 0 {
		ctxLen = DefaultContext
	}
	hw := o.Hardware
	if hw == nil {
		hw = hardware.Detect()
	}
	var out []Model
	seen := map[string]bool{}
	add := func(m Model) {
		key := m.Path
		if real, err := filepath.EvalSymlinks(m.Path); err == nil {
			key = real
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, m)
	}

	var ollama []string
	if !o.SkipOllama {
		ollama = store.ListOllamaModels()
	}
	for _, ref := range ollama {
		rm, err := store.ResolveOllama(ref)
		if err != nil {
			add(Model{Ref: "ollama:" + ref, Origin: OriginOllama, Name: ref, Error: err.Error()})
			continue
		}
		m := describe(rm.Path, "ollama:"+ref, OriginOllama, rm.Size, ctxLen, hw)
		m.Name = ref
		// Older models do not record these in the file; Ollama's own record does.
		if m.Parameters == "" {
			m.Parameters = rm.Params
		}
		if m.Quantization == "" {
			m.Quantization = rm.Quant
		}
		add(m)
	}
	if o.Store != nil {
		for source, digest := range o.Store.RecordedDownloads() {
			p := o.Store.BlobPath(digest)
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			m := describe(p, source, OriginStore, info.Size(), ctxLen, hw)
			if m.Name == "" {
				m.Name = filepath.Base(source)
			}
			add(m)
		}
	}
	for _, dir := range o.Dirs {
		for _, p := range findGGUF(dir) {
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			m := describe(p, p, OriginFolder, info.Size(), ctxLen, hw)
			if m.Architecture == "clip" { // a vision projector, not a model
				continue
			}
			add(m)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Error == "") != (out[j].Error == "") {
			return out[i].Error == ""
		}
		return out[i].Ref < out[j].Ref
	})
	return out
}

var shardName = regexp.MustCompile(`-(\d{5})-of-\d{5}\.gguf$`)

// findGGUF returns the model files under dir, following symlinked files (a
// Hugging Face cache is made of them) but never symlinked folders, and skipping
// every shard after the first.
func findGGUF(dir string) []string {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	var found []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable folders are skipped
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= maxFolderDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".gguf") {
			return nil
		}
		if m := shardName.FindStringSubmatch(p); m != nil && m[1] != "00001" {
			return nil
		}
		if len(found) >= maxFolderFiles {
			return filepath.SkipAll
		}
		found = append(found, p)
		return nil
	})
	return found
}

func describe(path, ref, origin string, size int64, ctxLen int, hw *hardware.Report) Model {
	m := Model{Ref: ref, Origin: origin, Path: path, SizeBytes: size}
	h, err := ReadHeader(path)
	if err != nil {
		m.Error = err.Error()
		return m
	}
	m.Name = h.Name
	if m.Name == "" {
		m.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	m.Architecture = h.Architecture
	m.Parameters = h.SizeLabel
	m.Quantization = h.Quantization()
	m.ContextLen = h.Context
	m.Chat = h.HasChat
	m.MoE = h.Experts > 1
	m.Fit = Estimate(h, size, ctxLen, hw)
	return m
}

const mb = int64(1 << 20)

// runtimeOverheadMB covers compute buffers and the runtime itself.
const runtimeOverheadMB = 512

// NeedMB estimates the memory to run a model at the given context: its weights,
// an f16 key/value cache, and a fixed overhead. It is an estimate, not a
// measurement.
func NeedMB(h *Header, sizeBytes int64, ctxLen int) int64 {
	need := sizeBytes/mb + runtimeOverheadMB
	if h != nil && h.Layers > 0 && h.Heads > 0 && h.Embedding > 0 {
		kvHeads := h.KVHeads
		if kvHeads == 0 {
			kvHeads = h.Heads
		}
		ctx := uint64(ctxLen)
		if h.Context > 0 && h.Context < ctx {
			ctx = h.Context
		}
		headDim := h.Embedding / h.Heads
		need += int64(2*h.Layers*ctx*kvHeads*headDim*2) / mb
	}
	return need
}

// Estimate judges a model against each device that can run it.
func Estimate(h *Header, sizeBytes int64, ctxLen int, hw *hardware.Report) []Fit {
	need := NeedMB(h, sizeBytes, ctxLen)
	verdict := func(have int64) string {
		switch {
		case have <= 0:
			return "unknown"
		case float64(need) <= 0.8*float64(have):
			return "fits"
		case need <= have:
			return "tight"
		}
		return "too_large"
	}
	var fits []Fit
	for _, d := range hw.Devices {
		switch d.Class {
		case hardware.ClassGPU:
			have := d.MemoryMB
			if have == 0 && d.Vendor == "apple" { // unified memory
				have = hw.TotalRAMMB
			}
			fits = append(fits, Fit{Device: "gpu", MemoryMB: have, NeedMB: need, Verdict: verdict(have)})
		case hardware.ClassCPU:
			fits = append(fits, Fit{Device: "cpu", MemoryMB: hw.TotalRAMMB, NeedMB: need, Verdict: verdict(hw.TotalRAMMB)})
		}
	}
	return fits
}

// Describe is a one-line summary for terminals.
func (m Model) Describe() string {
	parts := []string{m.Ref}
	if m.Parameters != "" {
		parts = append(parts, m.Parameters)
	}
	if m.Quantization != "" {
		parts = append(parts, m.Quantization)
	}
	parts = append(parts, fmt.Sprintf("%.1f GB", float64(m.SizeBytes)/(1<<30)))
	return strings.Join(parts, "  ")
}
