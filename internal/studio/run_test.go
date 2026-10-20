package studio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeExe stands in for the nexus executable, so the endpoints are tested for
// what they pass to it and what they do with its output, not for model speed.
func fakeExe(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	p := filepath.Join(t.TempDir(), "fake-nexus")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

const happy = `
echo "$@" > "$ARGS_FILE"
case "$1" in
compose)
  while [ $# -gt 0 ]; do [ "$1" = "--input-file" ] && f="$2"; shift; done
  echo 'this line is not an event'
  echo '{"event":"start","order":["a"]}'
  printf '{"event":"agent_done","agent":"a","output":"%s"}\n' "$(cat "$f")"
  echo '{"event":"done","ok":true}'
  ;;
run)
  cp "$2/nexus.yaml" "$UNIT_FILE"
  echo '{"text":"ready","tokens_out":2,"eval_tokens_per_sec":9.5,"load_ms":120,"total_ms":300,"device":"cpu","backend":"llama.cpp"}'
  ;;
esac
`

func TestRunStreamsOnlyWellFormedEventsAndKeepsInputOffTheCommandLine(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_FILE", args)
	e := newEnvExe(t, fakeExe(t, happy))
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/run", strings.NewReader(`{"input":"top secret question","max_tokens":999999}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || strings.Contains(buf.String(), "not an event") {
		t.Fatalf("want exactly the three event lines, got %q", lines)
	}
	if !strings.Contains(lines[1], "top secret question") || !strings.Contains(lines[2], `"ok":true`) {
		t.Errorf("events = %q", lines)
	}
	got, _ := os.ReadFile(args)
	if strings.Contains(string(got), "secret") {
		t.Errorf("the input must not be on the command line: %s", got)
	}
	for _, want := range []string{"compose up -f " + e.root, "--events", "-n 4096"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("command %q lacks %q", got, want)
		}
	}
}

func TestRunExplainsAFailureThatHappensBeforeTheWorkflowStarts(t *testing.T) {
	e := newEnvExe(t, fakeExe(t, "echo 'model ollama:nope not found' >&2\nexit 1\n"))
	code, out := e.do("POST", "/api/run", map[string]any{"input": "x"})
	var ev struct {
		Event, Error string
		OK           *bool
	}
	json.Unmarshal(bytes.TrimSpace(out), &ev)
	if code != 200 || ev.Event != "done" || ev.OK == nil || *ev.OK || !strings.Contains(ev.Error, "not found") {
		t.Fatalf("got %d %s", code, out)
	}
}

func TestOnlyOneRunAtATimeAndClosingThePageStopsIt(t *testing.T) {
	e := newEnvExe(t, fakeExe(t, "echo '{\"event\":\"start\",\"order\":[\"a\"]}'\nexec sleep 30\n"))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/api/run", strings.NewReader(`{"input":"x"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	if n, _ := resp.Body.Read(buf); n == 0 {
		t.Fatal("expected the first event")
	}
	// probe starts a run and stops it again at once, returning the status it got.
	probe := func() int {
		pctx, pcancel := context.WithCancel(context.Background())
		defer pcancel()
		r, _ := http.NewRequestWithContext(pctx, "POST", e.srv.URL+"/api/run", strings.NewReader(`{"input":"y"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		rsp, err := http.DefaultClient.Do(r)
		if err != nil {
			return 0
		}
		rsp.Body.Close()
		return rsp.StatusCode
	}
	if code := probe(); code != 409 {
		t.Errorf("a second run must be refused, got %d", code)
	}
	cancel() // the page closed

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if probe() == 200 {
			return // the interrupted run released the slot
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the run did not stop when the page closed")
}

func writeGGUF(t *testing.T, path string) {
	t.Helper()
	var b bytes.Buffer
	str := func(s string) { binary.Write(&b, binary.LittleEndian, uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	binary.Write(&b, binary.LittleEndian, uint64(0))
	binary.Write(&b, binary.LittleEndian, uint64(2))
	for _, kv := range [][2]string{{"general.architecture", "llama"}, {"general.name", "Tiny"}} {
		str(kv[0])
		binary.Write(&b, binary.LittleEndian, uint32(8))
		str(kv[1])
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTestModelRunsOnlyModelsFromTheList(t *testing.T) {
	unit := filepath.Join(t.TempDir(), "unit.yaml")
	t.Setenv("ARGS_FILE", filepath.Join(t.TempDir(), "args"))
	t.Setenv("UNIT_FILE", unit)
	e := newEnvExe(t, fakeExe(t, happy))
	dir := t.TempDir()
	model := filepath.Join(dir, "tiny.gguf")
	writeGGUF(t, model)
	if c, out := e.do("PUT", "/api/settings", map[string]any{"model_dirs": []string{dir}}); c != 200 {
		t.Fatalf("settings: %d %s", c, out)
	}

	code, out := e.do("POST", "/api/test-model", map[string]any{"ref": model})
	var r modelTest
	json.Unmarshal(out, &r)
	if code != 200 || !r.OK || r.Reply != "ready" || r.TokPerS != 9.5 || r.Device != "cpu" || r.LoadMS != 120 {
		t.Fatalf("test = %d %s", code, out)
	}
	if b, _ := os.ReadFile(unit); !strings.Contains(string(b), "source: "+model) || !strings.Contains(string(b), "type: chat") {
		t.Errorf("the test unit must name the chosen model:\n%s", b)
	}

	for _, ref := range []string{"/etc/passwd", "https://example.com/model.gguf", "../tiny.gguf", "ollama:not-installed:latest", ""} {
		if c, _ := e.do("POST", "/api/test-model", map[string]any{"ref": ref}); c != 404 {
			t.Errorf("ref %q: status %d, want 404", ref, c)
		}
	}
}

func TestTestModelReportsWhyAModelFailed(t *testing.T) {
	e := newEnvExe(t, fakeExe(t, "echo 'out of memory loading weights' >&2\nexit 1\n"))
	dir := t.TempDir()
	model := filepath.Join(dir, "big.gguf")
	writeGGUF(t, model)
	e.do("PUT", "/api/settings", map[string]any{"model_dirs": []string{dir}})
	_, out := e.do("POST", "/api/test-model", map[string]any{"ref": model})
	var r modelTest
	json.Unmarshal(out, &r)
	if r.OK || !strings.Contains(r.Error, "out of memory") {
		t.Errorf("result = %s", out)
	}
}
