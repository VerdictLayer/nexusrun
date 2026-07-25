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
	// An explicit override is taken at its word by binary(), so a typo in
	// NEXUSRUN_LLAMA_SERVER would otherwise be reported as "ready" here and
	// fail much later, at generation, as a fork/exec error.
	if _, err := os.Stat(bin); err != nil {
		c.Detail = fmt.Sprintf("llama-server at %s is not usable: %v", bin, err)
		return c
	}
	c.Available = true
	c.AcceptsModelPath = true
	// The OpenAI-compatible endpoint carries tool calls natively, and it is
	// the only local GGUF path that does — llama-cli has no equivalent.
	c.SupportsTools = true
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
	// Tool calling needs the model's own Jinja chat template: without
	// --jinja, llama-server falls back to its built-in templates, which
	// have no notion of tool calls, and the tools are quietly ignored.
	inst, err := l.acquire(ctx, req.ModelPath, req.Device, req.Context, len(req.Tools) > 0)
	if err != nil {
		return nil, err
	}
	start := time.Now()

	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 256
	}

	url, body, err := serverRequest(req, maxTok)
	if err != nil {
		return nil, err
	}
	raw, err := postJSON(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", inst.port, url), body)
	if err != nil {
		return nil, err
	}
	reply, err := parseServerReply(raw)
	if err != nil {
		return nil, err
	}
	if req.Stream != nil {
		req.Stream(reply.Text)
	}

	return &Result{
		Text:      strings.TrimSpace(reply.Text),
		ToolCalls: reply.ToolCalls,
		Backend:   l.Name(),
		Device:    req.Device,
		TokensOut: reply.Tokens,
		PromptTPS: reply.PromptTPS,
		EvalTPS:   reply.EvalTPS,
		WallTime:  time.Since(start),
	}, nil
}

// serverRequest builds the endpoint path and body for one request.
//
// It is separate from Generate so the wire format can be tested without
// starting a real llama-server and loading multi-gigabyte weights.
func serverRequest(req Request, maxTok int) (path string, body map[string]any, err error) {
	// Declaring tools implies a conversation: there is no turn on the
	// completion endpoint for a tool call to occupy, so tools route here
	// whether or not the caller set Chat.
	if req.Chat || len(req.Messages) > 0 || len(req.Tools) > 0 {
		// The OpenAI-compatible endpoint applies the model's own chat
		// template, which is what instruction-tuned models expect, and is
		// the only one that carries tool calls.
		body = map[string]any{
			"messages":    messagesWire(req.Conversation()),
			"max_tokens":  maxTok,
			"temperature": req.Temperature,
		}
		if tools := toolsWire(req.Tools); tools != nil {
			body["tools"] = tools
		}
		return "/v1/chat/completions", body, nil
	}
	prompt := req.Prompt
	if req.System != "" {
		prompt = req.System + "\n\n" + req.Prompt
	}
	return "/completion", map[string]any{
		"prompt":      prompt,
		"n_predict":   maxTok,
		"temperature": req.Temperature,
	}, nil
}

// serverReply is the decoded response, from either endpoint.
type serverReply struct {
	Text      string
	ToolCalls []ToolCall
	Tokens    int
	PromptTPS float64
	EvalTPS   float64
}

func parseServerReply(raw []byte) (*serverReply, error) {
	var parsed struct {
		Content string `json:"content"`
		Choices []struct {
			Message struct {
				Content   string         `json:"content"`
				ToolCalls []wireToolCall `json:"tool_calls"`
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

	r := &serverReply{
		Text:      parsed.Content,
		Tokens:    parsed.Timings.PredictedN,
		PromptTPS: parsed.Timings.PromptPerSecond,
		EvalTPS:   parsed.Timings.PredictedPerSecond,
	}
	if len(parsed.Choices) > 0 {
		r.Text = parsed.Choices[0].Message.Content
		r.ToolCalls = toolCallsFrom(parsed.Choices[0].Message.ToolCalls)
	}
	if r.Tokens == 0 {
		r.Tokens = parsed.Usage.CompletionTokens
	}
	return r, nil
}

// instanceKey identifies a running server. Jinja is part of it because
// --jinja changes how prompts are templated: a tool-calling request must
// not be answered by a server started without it, and a plain chat request
// must not silently change templating because a tool run came first.
func instanceKey(modelPath, device string, jinja bool) string {
	key := modelPath + "|" + device
	if jinja {
		key += "|jinja"
	}
	return key
}

// acquire returns a ready server for the model, reusing a running one.
func (l *LlamaServer) acquire(ctx context.Context, modelPath, device string, ctxSize int, jinja bool) (*serverInstance, error) {
	bin := l.binary()
	if bin == "" {
		return nil, errors.New("llama-server not found")
	}

	key := instanceKey(modelPath, device, jinja)
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

	args := []string{
		"-m", modelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"-c", strconv.Itoa(ctxSize),
		"-ngl", ngl,
	}
	if jinja {
		args = append(args, "--jinja")
	}
	cmd := runWithLibPath(bin, args...)
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

// IsWarm reports whether a model is already resident for a device. It asks
// about the plain (non-jinja) instance, which is what the warm-model daemon
// serves.
func (l *LlamaServer) IsWarm(modelPath, device string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	inst, ok := l.instances[instanceKey(modelPath, device, false)]
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
