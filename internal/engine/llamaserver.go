package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/verdictlayer/nexusrun/internal/hardware"
)

// LlamaServer drives llama.cpp's HTTP server instead of its one-shot CLI.
//
// The difference that matters is model load time. `llama-cli` reloads the
// weights on every invocation — about 1.3s for a 1.5B model and far worse
// for large ones. A server holds them in memory, so the second and later
// requests skip that cost entirely. It also applies the model's chat
// template server-side via the OpenAI-compatible endpoint.
type LlamaServer struct {
	bin string

	mu        sync.Mutex
	instances map[string]*serverInstance
}

// serverInstance is one running llama-server process holding one model.
type serverInstance struct {
	port     int
	cmd      *exec.Cmd
	model    string
	device   string
	lastUsed time.Time
	ready    bool
}

func (l *LlamaServer) Name() string { return "llama.cpp/server" }

func (l *LlamaServer) binary() string {
	if l.bin != "" {
		return l.bin
	}
	if v := os.Getenv("NEXUSRUN_LLAMA_SERVER"); v != "" {
		l.bin = v
		return v
	}
	if p, err := exec.LookPath("llama-server"); err == nil {
		l.bin = p
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "llama-server"),
		filepath.Join(home, ".local", "lib", "llama.cpp", "llama-server"),
		"/usr/local/bin/llama-server",
		"/opt/homebrew/bin/llama-server",
	} {
		if _, err := os.Stat(p); err == nil {
			l.bin = p
			return p
		}
	}
	// Last resort: llama-server is built alongside llama-cli, so if the
	// CLI is on PATH the server is almost certainly its sibling. This
	// matters because people commonly symlink only llama-cli into their
	// PATH, which silently costs them warm models for no reason.
	if sib := siblingOf((&LlamaCPP{}).binary(), "llama-server"); sib != "" {
		l.bin = sib
		return sib
	}
	return ""
}

// siblingOf resolves a binary path through any symlinks and looks for
// name next to it. Returns "" if there is no such executable.
func siblingOf(bin, name string) string {
	if bin == "" {
		return ""
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	cand := filepath.Join(filepath.Dir(real), name)
	if runtime.GOOS == "windows" {
		cand += ".exe"
	}
	if info, err := os.Stat(cand); err == nil && !info.IsDir() {
		return cand
	}
	return ""
}

func (l *LlamaServer) Probe() Capability {
	c := Capability{Backend: l.Name()}
	bin := l.binary()
	if bin == "" {
		c.Detail = "llama-server not found (set NEXUSRUN_LLAMA_SERVER to enable warm models)"
		return c
	}
	c.Available = true
	c.AcceptsModelPath = true
	c.Devices = []string{hardware.ClassCPU}

	if out, err := runWithLibPath(bin, "--version").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "version:") {
				c.Version = strings.TrimSpace(strings.TrimPrefix(line, "version:"))
				break
			}
		}
	}
	if devices := probeLlamaDevices(bin); len(devices) > 0 {
		c.Devices = append([]string{hardware.ClassGPU}, c.Devices...)
		c.Detail = "GPU offload available; keeps models warm between runs"
	} else {
		c.Detail = "CPU-only build; keeps models warm between runs"
	}
	return c
}

// Generate sends a request to a warm server, starting one if needed.
func (l *LlamaServer) Generate(ctx context.Context, req Request) (*Result, error) {
	inst, err := l.acquire(ctx, req.ModelPath, req.Device, req.Context)
	if err != nil {
		return nil, err
	}
	start := time.Now()

	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 256
	}

	var (
		url  string
		body any
	)
	if req.Chat {
		// The OpenAI-compatible endpoint applies the model's own chat
		// template, which is what instruction-tuned models expect.
		msgs := []map[string]string{}
		if req.System != "" {
			msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
		}
		msgs = append(msgs, map[string]string{"role": "user", "content": req.Prompt})
		url = fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", inst.port)
		body = map[string]any{
			"messages":    msgs,
			"max_tokens":  maxTok,
			"temperature": req.Temperature,
		}
	} else {
		prompt := req.Prompt
		if req.System != "" {
			prompt = req.System + "\n\n" + req.Prompt
		}
		url = fmt.Sprintf("http://127.0.0.1:%d/completion", inst.port)
		body = map[string]any{
			"prompt":      prompt,
			"n_predict":   maxTok,
			"temperature": req.Temperature,
		}
	}

	raw, err := postJSON(ctx, url, body)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Content string `json:"content"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Timings struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
			PredictedN         int     `json:"predicted_n"`
		} `json:"timings"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode llama-server response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("llama-server: %s", parsed.Error.Message)
	}

	text := parsed.Content
	if len(parsed.Choices) > 0 {
		text = parsed.Choices[0].Message.Content
	}
	tokens := parsed.Timings.PredictedN
	if tokens == 0 {
		tokens = parsed.Usage.CompletionTokens
	}
	if req.Stream != nil {
		req.Stream(text)
	}

	return &Result{
		Text:      strings.TrimSpace(text),
		Backend:   l.Name(),
		Device:    req.Device,
		TokensOut: tokens,
		PromptTPS: parsed.Timings.PromptPerSecond,
		EvalTPS:   parsed.Timings.PredictedPerSecond,
		WallTime:  time.Since(start),
	}, nil
}

// acquire returns a ready server for the model, reusing a running one.
func (l *LlamaServer) acquire(ctx context.Context, modelPath, device string, ctxSize int) (*serverInstance, error) {
	bin := l.binary()
	if bin == "" {
		return nil, errors.New("llama-server not found")
	}

	key := modelPath + "|" + device
	l.mu.Lock()
	if l.instances == nil {
		l.instances = map[string]*serverInstance{}
	}
	if inst, ok := l.instances[key]; ok && inst.ready && inst.alive() {
		inst.lastUsed = time.Now()
		l.mu.Unlock()
		return inst, nil
	}
	l.mu.Unlock()

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	ngl := "0"
	if device == hardware.ClassGPU {
		ngl = "999"
	}
	if ctxSize <= 0 {
		ctxSize = 4096
	}

	cmd := runWithLibPath(bin,
		"-m", modelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"-c", strconv.Itoa(ctxSize),
		"-ngl", ngl,
	)
	logFile, _ := os.CreateTemp("", "nexus-llama-server-*.log")
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start llama-server: %w", err)
	}

	inst := &serverInstance{port: port, cmd: cmd, model: modelPath, device: device, lastUsed: time.Now()}
	if err := inst.waitReady(ctx); err != nil {
		_ = cmd.Process.Kill()
		detail := ""
		if logFile != nil {
			if b, rerr := os.ReadFile(logFile.Name()); rerr == nil {
				detail = "\n" + tail(string(b), 12)
			}
		}
		return nil, fmt.Errorf("llama-server did not become ready: %w%s", err, detail)
	}
	inst.ready = true

	l.mu.Lock()
	l.instances[key] = inst
	l.mu.Unlock()
	return inst, nil
}

// waitReady polls /health until the model finishes loading. The endpoint
// answers 503 with "Loading model" until then, so only a 200 counts.
func (inst *serverInstance) waitReady(ctx context.Context) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", inst.port)
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !inst.alive() {
			return errors.New("process exited during startup")
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := defaultHTTP.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("timed out waiting for model to load")
}

func (inst *serverInstance) alive() bool {
	return inst.cmd != nil && inst.cmd.Process != nil && inst.cmd.ProcessState == nil
}

// Shutdown stops every running server. Callers that start servers must
// call this, or the processes outlive the CLI.
func (l *LlamaServer) Shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, inst := range l.instances {
		if inst.cmd != nil && inst.cmd.Process != nil {
			_ = inst.cmd.Process.Kill()
			_, _ = inst.cmd.Process.Wait()
		}
		delete(l.instances, key)
	}
}

// EvictIdle stops servers unused for longer than idle, freeing the memory
// their weights occupy.
func (l *LlamaServer) EvictIdle(idle time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	evicted := 0
	for key, inst := range l.instances {
		if time.Since(inst.lastUsed) > idle {
			if inst.cmd != nil && inst.cmd.Process != nil {
				_ = inst.cmd.Process.Kill()
				_, _ = inst.cmd.Process.Wait()
			}
			delete(l.instances, key)
			evicted++
		}
	}
	return evicted
}

// IsWarm reports whether a model is already resident for a device.
func (l *LlamaServer) IsWarm(modelPath, device string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	inst, ok := l.instances[modelPath+"|"+device]
	return ok && inst.ready && inst.alive()
}

// Warm reports the models currently held in memory.
func (l *LlamaServer) Warm() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, inst := range l.instances {
		out = append(out, fmt.Sprintf("%s (%s, port %d)", filepath.Base(inst.model), inst.device, inst.port))
	}
	return out
}

// --- helpers --------------------------------------------------------------

// runWithLibPath builds a command that can find sibling shared libraries.
// Prebuilt llama.cpp releases ship libggml*.so next to the binary rather
// than installing them system-wide.
func runWithLibPath(bin string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	dir := filepath.Dir(bin)
	env := os.Environ()
	existing := os.Getenv("LD_LIBRARY_PATH")
	if existing != "" {
		dir = dir + string(os.PathListSeparator) + existing
	}
	cmd.Env = append(env, "LD_LIBRARY_PATH="+dir)
	return cmd
}

// probeLlamaDevices returns accelerator classes a llama.cpp binary was
// compiled to drive, by parsing --list-devices.
func probeLlamaDevices(bin string) []string {
	out, err := runWithLibPath(bin, "--list-devices").CombinedOutput()
	if err != nil {
		return nil
	}
	var devices []string
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
			devices = append(devices, hardware.ClassGPU)
		}
	}
	return devices
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func postJSON(ctx context.Context, url string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
