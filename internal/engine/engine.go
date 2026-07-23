// Package engine executes models on a chosen device.
//
// A core design principle: detecting hardware is not enough. A machine
// can report an NPU or a GPU that the installed backend was never
// compiled to drive, or whose driver is broken. Every backend therefore
// exposes Probe(), which reports the devices it can *actually* execute
// on today. Scheduling uses the intersection of detected hardware and
// probed backend capability, never hardware presence alone.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/hardware"
)

// Request is a single generation call.
type Request struct {
	ModelPath   string
	Prompt      string
	System      string
	MaxTokens   int
	Temperature float64
	Context     int

	// Device selects the accelerator class ("npu", "gpu", "cpu").
	Device string

	// ModelRef is the unit's original model source (e.g. "ollama:phi3").
	// Backends that address models by name rather than path need it.
	ModelRef string

	// Chat applies the model's own chat template and runs a single turn.
	// Without it the system prompt is only concatenated ahead of the
	// user prompt, which instruction-tuned models largely ignore —
	// they continue the text instead of answering it.
	Chat bool

	// Stream, when non-nil, receives tokens as they are produced.
	Stream func(chunk string)
}

// Result carries the output and measured performance of a generation.
//
// Field names are snake_case on the wire to match the rest of the JSON
// surface (bench.DeviceResult, daemon.RunResponse) — `nexus run --json`
// reaches this struct directly and daemon.RunResponse when a warm pool
// answers, so the two paths must not disagree on casing.
type Result struct {
	Text      string        `json:"text"`
	Backend   string        `json:"backend"`
	Device    string        `json:"device"`
	TokensOut int           `json:"tokens_out"`
	PromptTPS float64       `json:"prompt_tokens_per_sec"`
	EvalTPS   float64       `json:"eval_tokens_per_sec"`
	LoadMS    float64       `json:"load_ms"`
	TotalMS   float64       `json:"total_ms"`
	WallTime  time.Duration `json:"wall_time"`
}

// Capability describes what a backend can execute on this host.
//
// The json tags are load-bearing: this struct is served by
// GET /api/hardware and printed by `nexus doctor --json`, and the web
// console reads these exact lowercase keys.
type Capability struct {
	Backend   string   `json:"backend"`   // llama.cpp, ollama, onnxruntime
	Available bool     `json:"available"` // backend is installed and runnable
	Devices   []string `json:"devices"`   // accelerator classes it can actually drive
	Version   string   `json:"version"`
	Detail    string   `json:"detail"` // why unavailable, or extra context

	// AcceptsModelPath reports whether the backend can execute a model
	// given a file path. Backends that only take model names (Ollama)
	// cannot run arbitrary unit weights and are excluded from
	// path-based scheduling and benchmarking.
	AcceptsModelPath bool `json:"accepts_model_path"`
}

// Backend executes models.
type Backend interface {
	Name() string
	Probe() Capability
	Generate(ctx context.Context, req Request) (*Result, error)
}

// All returns the backends used for one-shot scheduling, in priority
// order. LlamaServer is deliberately absent: starting a server, running
// one prompt, and killing it is strictly worse than the CLI. It earns
// its place only inside a long-lived daemon, which constructs it
// directly — see internal/daemon.
func All() []Backend {
	return []Backend{&LlamaCPP{}, &Ollama{}, &ONNXRuntime{}}
}

// ProbeAll reports capability for every backend that exists, including
// ones not used for one-shot scheduling. Diagnostics want the full list.
func ProbeAll() []Capability {
	all := []Backend{&LlamaCPP{}, &LlamaServer{}, &Ollama{}, &ONNXRuntime{}}
	caps := make([]Capability, 0, len(all))
	for _, b := range all {
		caps = append(caps, b.Probe())
	}
	return caps
}

// Select picks the best backend/device pair for a preference order,
// intersecting detected hardware with probed backend capability.
func Select(hw *hardware.Report, prefer []string) (Backend, string, error) {
	return SelectFrom(All(), hw, prefer)
}

// SelectFrom is Select over an explicit backend list. Taking the list as
// a parameter is what makes accelerator scheduling testable: NPU and GPU
// paths can be exercised with stub backends on a machine that has
// neither, which is otherwise impossible to cover.
func SelectFrom(candidates []Backend, hw *hardware.Report, prefer []string) (Backend, string, error) {
	caps := map[string]Capability{}
	var backends []Backend
	for _, b := range candidates {
		c := b.Probe()
		caps[b.Name()] = c
		if c.Available && c.AcceptsModelPath {
			backends = append(backends, b)
		}
	}
	if len(backends) == 0 {
		var why []string
		for name, c := range caps {
			why = append(why, fmt.Sprintf("%s: %s", name, c.Detail))
		}
		return nil, "", fmt.Errorf("no execution backend available:\n  %s", strings.Join(why, "\n  "))
	}
	for _, class := range prefer {
		if !hw.Has(class) {
			continue
		}
		for _, b := range backends {
			for _, d := range caps[b.Name()].Devices {
				if d == class {
					return b, class, nil
				}
			}
		}
	}
	// Nothing matched the preference; fall back to the first backend's
	// most capable device.
	b := backends[0]
	devs := caps[b.Name()].Devices
	if len(devs) == 0 {
		return nil, "", fmt.Errorf("backend %s reports no usable devices", b.Name())
	}
	return b, devs[0], nil
}

// --- llama.cpp ------------------------------------------------------------

// LlamaCPP drives the llama-cli binary, the most portable GGUF runtime.
type LlamaCPP struct{ bin string }

func (l *LlamaCPP) Name() string { return "llama.cpp" }

func (l *LlamaCPP) binary() string {
	if l.bin != "" {
		return l.bin
	}
	if v := os.Getenv("NEXUSRUN_LLAMA_CLI"); v != "" {
		l.bin = v
		return v
	}
	for _, name := range []string{"llama-cli", "llama"} {
		if p, err := exec.LookPath(name); err == nil {
			l.bin = p
			return p
		}
	}
	// Common manual-install locations.
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "llama-cli"),
		"/usr/local/bin/llama-cli",
		"/opt/homebrew/bin/llama-cli",
	} {
		if _, err := os.Stat(p); err == nil {
			l.bin = p
			return p
		}
	}
	return ""
}

var reListDevice = regexp.MustCompile(`^\s{2,}(\S+):\s*(.+)$`)

// Probe asks llama-cli which devices it was compiled to use. An empty
// device list means a CPU-only build, regardless of the GPU or NPU
// physically present in the machine.
func (l *LlamaCPP) Probe() Capability {
	c := Capability{Backend: l.Name()}
	bin := l.binary()
	if bin == "" {
		c.Detail = "llama-cli not found in PATH (install llama.cpp or set NEXUSRUN_LLAMA_CLI)"
		return c
	}
	c.Available = true
	c.AcceptsModelPath = true
	c.Devices = []string{hardware.ClassCPU} // every build can run on CPU

	if out, err := exec.Command(bin, "--version").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "version:") {
				c.Version = strings.TrimSpace(strings.TrimPrefix(line, "version:"))
				break
			}
		}
	}

	out, err := exec.Command(bin, "--list-devices").CombinedOutput()
	if err != nil {
		c.Detail = "device list unavailable; assuming CPU-only"
		return c
	}
	var accel []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Available devices") || strings.TrimSpace(line) == "" {
			continue
		}
		m := reListDevice.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := strings.ToLower(m[1] + " " + m[2])
		switch {
		case strings.Contains(name, "cuda"), strings.Contains(name, "rocm"),
			strings.Contains(name, "metal"), strings.Contains(name, "vulkan"),
			strings.Contains(name, "sycl"):
			accel = append(accel, hardware.ClassGPU)
		}
	}
	if len(accel) > 0 {
		c.Devices = append([]string{hardware.ClassGPU}, c.Devices...)
		c.Detail = "GPU offload available"
	} else {
		c.Detail = "CPU-only build (no GPU backend compiled in)"
	}
	return c
}

var (
	rePromptEval = regexp.MustCompile(`prompt eval time =\s*([\d.]+) ms /\s*(\d+) tokens.*?([\d.]+) tokens per second`)
	reEval       = regexp.MustCompile(`(?m)^llama_perf_context_print:\s+eval time =\s*([\d.]+) ms /\s*(\d+) runs.*?([\d.]+) tokens per second`)
	reLoad       = regexp.MustCompile(`load time =\s*([\d.]+) ms`)
	reTotal      = regexp.MustCompile(`total time =\s*([\d.]+) ms`)
)

func (l *LlamaCPP) Generate(ctx context.Context, req Request) (*Result, error) {
	bin := l.binary()
	if bin == "" {
		return nil, errors.New("llama-cli not found")
	}

	ngl := "0"
	if req.Device == hardware.ClassGPU {
		ngl = "999"
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 256
	}
	ctxSize := req.Context
	if ctxSize <= 0 {
		ctxSize = 4096
	}

	args := []string{
		"-m", req.ModelPath,
		"-n", strconv.Itoa(maxTok),
		"-ngl", ngl,
		"-c", strconv.Itoa(ctxSize),
		"--temp", strconv.FormatFloat(req.Temperature, 'f', 2, 64),
		"--no-warmup",
		"--no-display-prompt",
	}
	if req.Chat {
		// Single-turn conversation mode applies the model's chat
		// template, so instruction-tuned models answer the prompt
		// rather than autocompleting it.
		args = append(args, "-p", req.Prompt, "--single-turn")
		if req.System != "" {
			args = append(args, "-sys", req.System)
		}
	} else {
		// Raw completion: no template, system text simply leads.
		prompt := req.Prompt
		if req.System != "" {
			prompt = req.System + "\n\n" + req.Prompt
		}
		args = append(args, "-p", prompt, "-no-cnv")
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var text strings.Builder
	// The end-of-text marker must not reach the terminal. The stream
	// withholds a tail long enough to contain the marker plus the
	// newlines llama-cli prints after it, and releases it only once the
	// process has ended and the marker can be stripped.
	const holdN = len(llamaEOT) + 16
	var held string
	emit := func(chunk string) {
		if req.Stream == nil {
			return
		}
		held += chunk
		if len(held) <= holdN {
			return
		}
		out := held[:len(held)-holdN]
		held = held[len(held)-holdN:]
		req.Stream(out)
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			text.WriteString(chunk)
			emit(chunk)
		}
		if rerr != nil {
			break
		}
	}
	if req.Stream != nil {
		rest := strings.TrimRight(held, " \t\r\n")
		rest = strings.TrimSuffix(rest, llamaEOT)
		if rest = strings.TrimRight(rest, " \t\r\n"); rest != "" {
			req.Stream(rest)
		}
	}
	waitErr := cmd.Wait()
	wall := time.Since(start)
	if waitErr != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("llama-cli failed: %w\n%s", waitErr, tail(stderr.String(), 20))
	}

	res := &Result{
		Text:     cleanGenerated(text.String()),
		Backend:  l.Name(),
		Device:   req.Device,
		WallTime: wall,
	}
	perf := stderr.String()
	if m := rePromptEval.FindStringSubmatch(perf); m != nil {
		res.PromptTPS, _ = strconv.ParseFloat(m[3], 64)
	}
	if m := reEval.FindStringSubmatch(perf); m != nil {
		res.TokensOut, _ = strconv.Atoi(m[2])
		res.EvalTPS, _ = strconv.ParseFloat(m[3], 64)
	}
	if m := reLoad.FindStringSubmatch(perf); m != nil {
		res.LoadMS, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := reTotal.FindStringSubmatch(perf); m != nil {
		res.TotalMS, _ = strconv.ParseFloat(m[1], 64)
	}
	return res, nil
}

// --- Ollama ---------------------------------------------------------------

// Ollama drives a local Ollama server over HTTP. It is a convenience
// backend: it handles its own device placement, so NexusRun reports the
// device as whatever Ollama chose rather than controlling it.
type Ollama struct{}

func (o *Ollama) Name() string { return "ollama" }

func (o *Ollama) host() string {
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		if !strings.HasPrefix(v, "http") {
			return "http://" + v
		}
		return v
	}
	return "http://127.0.0.1:11434"
}

func (o *Ollama) Probe() Capability {
	c := Capability{Backend: o.Name()}
	client := &httpClient{timeout: 2 * time.Second}
	body, err := client.get(o.host() + "/api/version")
	if err != nil {
		c.Detail = "no Ollama server at " + o.host()
		return c
	}
	c.Available = true
	c.Devices = []string{hardware.ClassGPU, hardware.ClassCPU}
	c.Version = strings.Trim(extractJSONString(body, "version"), `"`)
	c.Detail = "server reachable; runs ollama: models warm, picks its own device"
	return c
}

// Generate runs a model by name through a local Ollama server. Ollama
// keeps weights resident between calls, so there is no reload cost.
func (o *Ollama) Generate(ctx context.Context, req Request) (*Result, error) {
	name := strings.TrimPrefix(req.ModelRef, "ollama:")
	if name == "" || !strings.HasPrefix(req.ModelRef, "ollama:") {
		return nil, errors.New("ollama backend addresses models by name; this unit's model is not an ollama: source")
	}

	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 256
	}
	options := map[string]any{
		"temperature": req.Temperature,
		"num_predict": maxTok,
	}
	if req.Context > 0 {
		options["num_ctx"] = req.Context
	}

	var url string
	var body map[string]any
	if req.Chat {
		msgs := []map[string]string{}
		if req.System != "" {
			msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
		}
		msgs = append(msgs, map[string]string{"role": "user", "content": req.Prompt})
		url = o.host() + "/api/chat"
		body = map[string]any{"model": name, "messages": msgs, "stream": false, "options": options}
	} else {
		url = o.host() + "/api/generate"
		body = map[string]any{"model": name, "prompt": req.Prompt, "stream": false, "options": options}
		if req.System != "" {
			body["system"] = req.System
		}
	}

	start := time.Now()
	raw, err := postJSON(ctx, url, body)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Response string `json:"response"`
		Message  struct {
			Content string `json:"content"`
		} `json:"message"`
		EvalCount       int    `json:"eval_count"`
		EvalDuration    int64  `json:"eval_duration"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		PromptEvalDur   int64  `json:"prompt_eval_duration"`
		Error           string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}
	if parsed.Error != "" {
		return nil, fmt.Errorf("ollama: %s", parsed.Error)
	}

	text := parsed.Response
	if text == "" {
		text = parsed.Message.Content
	}
	if req.Stream != nil {
		req.Stream(text)
	}

	res := &Result{
		Text:      strings.TrimSpace(text),
		Backend:   o.Name(),
		Device:    req.Device,
		TokensOut: parsed.EvalCount,
		WallTime:  time.Since(start),
	}
	// Ollama reports durations in nanoseconds.
	if parsed.EvalDuration > 0 {
		res.EvalTPS = float64(parsed.EvalCount) / (float64(parsed.EvalDuration) / 1e9)
	}
	if parsed.PromptEvalDur > 0 {
		res.PromptTPS = float64(parsed.PromptEvalCount) / (float64(parsed.PromptEvalDur) / 1e9)
	}
	return res, nil
}

// --- ONNX Runtime / NPU ---------------------------------------------------

// ONNXRuntime is the path to NPU execution. NPUs are not reachable
// through llama.cpp: each vendor ships its own execution provider —
// OpenVINO (Intel Core Ultra), QNN (Qualcomm Snapdragon X), VitisAI
// (AMD Ryzen AI), CoreML (Apple Neural Engine). This backend probes for
// the runtime library and the vendor EP that matches detected hardware.
type ONNXRuntime struct{}

func (o *ONNXRuntime) Name() string { return "onnxruntime" }

func (o *ONNXRuntime) Probe() Capability {
	c := Capability{Backend: o.Name()}
	lib := findONNXRuntime()
	if lib == "" {
		c.Detail = "onnxruntime shared library not found (needed for NPU execution)"
		return c
	}
	c.Available = true
	c.AcceptsModelPath = true
	c.Version = lib

	hw := hardware.Detect()
	for _, d := range hw.Devices {
		if d.Class == hardware.ClassNPU {
			c.Devices = append(c.Devices, hardware.ClassNPU)
			c.Detail = "NPU execution provider target: " + d.Backend
			break
		}
	}
	c.Devices = append(c.Devices, hardware.ClassCPU)
	if c.Detail == "" {
		c.Detail = "no NPU detected; CPU execution provider only"
	}
	return c
}

func (o *ONNXRuntime) Generate(ctx context.Context, req Request) (*Result, error) {
	return nil, errors.New(
		"onnxruntime generation is not wired up yet: it requires cgo bindings and an " +
			"ONNX-format model. GGUF units run on llama.cpp. Track NPU support in docs/NPU.md")
}

// findONNXRuntime locates libonnxruntime on the host.
func findONNXRuntime() string {
	var names []string
	switch runtime.GOOS {
	case "darwin":
		names = []string{"libonnxruntime.dylib"}
	case "windows":
		names = []string{"onnxruntime.dll"}
	default:
		names = []string{"libonnxruntime.so", "libonnxruntime.so.1"}
	}
	dirs := []string{"/usr/lib", "/usr/local/lib", "/usr/lib/x86_64-linux-gnu", "/opt/homebrew/lib"}
	if v := os.Getenv("NEXUSRUN_ONNXRUNTIME_LIB"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	for _, d := range dirs {
		for _, n := range names {
			p := filepath.Join(d, n)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// --- small helpers --------------------------------------------------------

type httpClient struct{ timeout time.Duration }

func (h *httpClient) get(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	req, err := newRequest(ctx, url)
	if err != nil {
		return "", err
	}
	resp, err := defaultHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return string(b), err
}

func extractJSONString(body, key string) string {
	idx := strings.Index(body, `"`+key+`"`)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(key)+2:]
	start := strings.Index(rest, `"`)
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// llamaEOT is the end-of-generation marker llama-cli prints on stdout
// after the model emits its EOS token. It is decoration, not model
// output, but it arrives on the same stream as the text.
const llamaEOT = "[end of text]"

// cleanGenerated strips llama-cli's terminal decoration from generated
// text. Left in, the marker is saved to run logs and — worse — piped
// into the next stage by `nexus compose`, where the model treats it as
// part of the input and echoes corrupted variants of it back.
func cleanGenerated(s string) string {
	s = strings.TrimSpace(s)
	for {
		trimmed := strings.TrimSuffix(s, llamaEOT)
		if trimmed == s {
			return s
		}
		s = strings.TrimSpace(trimmed)
	}
}

func tail(s string, lines int) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	var all []string
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
