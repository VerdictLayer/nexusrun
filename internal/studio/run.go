package studio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/modelscan"
)

// Running things from the editor is the one place Studio starts a process, so
// it is deliberately narrow: the program is this same executable, the arguments
// are fixed apart from a few validated values, the working folder is the
// project, only one run happens at a time, and closing the page stops it. There
// is no way to name a different program or pass extra flags.

const (
	maxInput        = 64 << 10
	defaultTokens   = 256
	maxTokens       = 4096
	maxStderr       = 64 << 10
	testTimeout     = 5 * time.Minute
	testPrompt      = "Reply with the single word: ready"
	testSystem      = "You answer as briefly as possible."
	interruptGrace  = 15 * time.Second
	maxEventLine    = 4 << 20
	stderrTailLines = 12
)

// tailWriter keeps the end of what a process wrote to stderr.
type tailWriter struct{ buf []byte }

func (t *tailWriter) Write(b []byte) (int, error) {
	t.buf = append(t.buf, b...)
	if len(t.buf) > maxStderr {
		t.buf = t.buf[len(t.buf)-maxStderr:]
	}
	return len(b), nil
}

func (t *tailWriter) tail() string {
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	if len(lines) > stderrTailLines {
		lines = lines[len(lines)-stderrTailLines:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// command builds a process that stops cleanly when ctx ends: first an
// interrupt, which the CLI turns into an orderly shutdown of any model server it
// started, and a kill only if that is ignored.
func (p *project) command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, p.exe, args...)
	cmd.Dir = dir
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill() // no interrupt on this platform
		}
		return nil
	}
	cmd.WaitDelay = interruptGrace
	return cmd
}

// run executes the workflow and streams its steps to the page as they happen,
// one JSON object per line.
func (p *project) run(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Input     string `json:"input"`
		MaxTokens int    `json:"max_tokens"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Input) > maxInput {
		fail(w, http.StatusBadRequest, "the input is larger than %d bytes", maxInput)
		return
	}
	tokens := in.MaxTokens
	if tokens <= 0 {
		tokens = defaultTokens
	}
	if tokens > maxTokens {
		tokens = maxTokens
	}
	if !p.busy.CompareAndSwap(false, true) {
		fail(w, http.StatusConflict, "something is already running; wait for it to finish or stop it")
		return
	}
	defer p.busy.Store(false)

	// The input goes in a private file, not on the command line, where other
	// accounts on this machine could read it in the process list.
	tmp, err := os.MkdirTemp("", "studio-run-*")
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer os.RemoveAll(tmp)
	inputFile := filepath.Join(tmp, "input.txt")
	if err := os.WriteFile(inputFile, []byte(in.Input), 0o600); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}

	cmd := p.command(r.Context(), p.root, "compose", "up", "-f", p.root, "--events",
		"--input-file", inputFile, "-n", strconv.Itoa(tokens))
	stderr := &tailWriter{}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		fail(w, http.StatusInternalServerError, "could not start: %v", err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	flush()

	sawDone := false
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), maxEventLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		// Only well-formed objects reach the page; anything else the process
		// prints is not part of the trace.
		var ev struct {
			Event string `json:"event"`
		}
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &ev) != nil {
			continue
		}
		sawDone = sawDone || ev.Event == "done"
		w.Write(append(line, '\n'))
		flush()
	}
	waitErr := cmd.Wait()
	if !sawDone && r.Context().Err() == nil {
		// It ended before the workflow started (a missing model, a bad unit):
		// say why, in the same shape as a finished run.
		msg := stderr.tail()
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		b, _ := json.Marshal(map[string]any{"event": "done", "ok": false, "error": msg})
		w.Write(append(b, '\n'))
		flush()
	}
}

type modelTest struct {
	OK       bool    `json:"ok"`
	Error    string  `json:"error,omitempty"`
	Reply    string  `json:"reply,omitempty"`
	Tokens   int     `json:"tokens,omitempty"`
	TokPerS  float64 `json:"tokens_per_sec,omitempty"`
	LoadMS   float64 `json:"load_ms,omitempty"`
	TotalMS  float64 `json:"total_ms,omitempty"`
	Device   string  `json:"device,omitempty"`
	Backend  string  `json:"backend,omitempty"`
	TookMS   int64   `json:"took_ms"`
	Model    string  `json:"model"`
	Prompted string  `json:"prompt"`
}

// testModel loads one of the listed models through the real run path and asks it
// for a one-word reply, so "this model works here" is something that was
// observed. The model must be one the scan found; a path or address from the
// page is never run.
func (p *project) testModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ref string `json:"ref"`
	}
	if !decode(w, r, &in) {
		return
	}
	var chosen *modelscan.Model
	for _, m := range modelscan.Scan(modelscan.Options{Store: p.store, Dirs: p.allDirs()}) {
		if m.Ref == in.Ref && m.Error == "" {
			m := m
			chosen = &m
			break
		}
	}
	if chosen == nil {
		fail(w, http.StatusNotFound, "that model is not in your list")
		return
	}
	if !p.busy.CompareAndSwap(false, true) {
		fail(w, http.StatusConflict, "something is already running; wait for it to finish or stop it")
		return
	}
	defer p.busy.Store(false)

	dir, err := os.MkdirTemp("", "studio-test-*")
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer os.RemoveAll(dir)
	unit := manifest.Manifest{
		APIVersion: manifest.APIVersion, Name: "model-test", Version: "0.0.1",
		Models:     []manifest.Model{{ID: "main", Source: chosen.Ref, Context: 2048}},
		Entrypoint: manifest.Entrypoint{Type: "chat", SystemPrompt: testSystem},
	}
	data, err := yaml.Marshal(unit)
	if err != nil || os.WriteFile(filepath.Join(dir, unitFile), data, 0o644) != nil {
		fail(w, http.StatusInternalServerError, "could not prepare the test")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	start := time.Now()
	cmd := p.command(ctx, dir, "run", dir, "-p", testPrompt, "-n", "8", "--json", "--no-daemon")
	var stdout bytes.Buffer
	stderr := &tailWriter{}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	res := modelTest{Model: chosen.Ref, Prompted: testPrompt}
	if err := cmd.Run(); err != nil {
		res.Error = stderr.tail()
		if ctx.Err() == context.DeadlineExceeded {
			res.Error = fmt.Sprintf("no answer within %s", testTimeout)
		} else if res.Error == "" {
			res.Error = err.Error()
		}
	} else {
		var got struct {
			Text      string  `json:"text"`
			TokensOut int     `json:"tokens_out"`
			EvalTPS   float64 `json:"eval_tokens_per_sec"`
			LoadMS    float64 `json:"load_ms"`
			TotalMS   float64 `json:"total_ms"`
			Device    string  `json:"device"`
			Backend   string  `json:"backend"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			res.Error = "the model ran but its result could not be read"
		} else if strings.TrimSpace(got.Text) == "" {
			res.Error = "the model loaded but produced no text"
		} else {
			res.OK = true
			res.Reply, res.Tokens, res.TokPerS = strings.TrimSpace(got.Text), got.TokensOut, got.EvalTPS
			res.LoadMS, res.TotalMS, res.Device, res.Backend = got.LoadMS, got.TotalMS, got.Device, got.Backend
		}
	}
	res.TookMS = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, res)
}
