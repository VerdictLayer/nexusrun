package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubExec returns an ExecFunc answering from a table, and a pointer to
// the call log so a test can assert on order and prompts.
func stubExec(outputs map[string]string, calls *[]AgentRequest) ExecFunc {
	return func(_ context.Context, req AgentRequest) (*AgentResult, error) {
		*calls = append(*calls, req)
		out, ok := outputs[req.Name]
		if !ok {
			return nil, fmt.Errorf("no stub output for %s", req.Name)
		}
		return &AgentResult{
			Output: out, TokensOut: len(out), TokPerSec: 12.5,
			Backend: "stub", Device: "cpu",
		}, nil
	}
}

func mustParse(t *testing.T, src string) *Spec {
	t.Helper()
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

const linear = `apiVersion: nexusrun.dev/v1
kind: Workflow
name: chain
version: 1.0.0
agents:
  first: {unit: a:1}
  second: {unit: b:1}
  third: {unit: c:1}
routing:
  - {from: first, to: second}
  - {from: second, to: third}
`

func TestRunPipesOutputForward(t *testing.T) {
	spec := mustParse(t, linear)
	var calls []AgentRequest
	res, err := Run(context.Background(), stubExec(map[string]string{
		"first": "one", "second": "two", "third": "three",
	}, &calls), RunOptions{Spec: spec, Input: "seed"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("expected 3 agent calls, got %d", len(calls))
	}
	if calls[0].Prompt != "seed" {
		t.Errorf("source prompt = %q, want the workflow input", calls[0].Prompt)
	}
	if calls[1].Prompt != "one" {
		t.Errorf("second prompt = %q, want first's output", calls[1].Prompt)
	}
	if calls[2].Prompt != "two" {
		t.Errorf("third prompt = %q, want second's output", calls[2].Prompt)
	}
	if res.Output != "three" {
		t.Errorf("workflow output = %q, want the sink's output", res.Output)
	}
	if len(res.Messages) != 2 {
		t.Errorf("expected 2 bus messages, got %d", len(res.Messages))
	}
}

func TestRunAppliesConditionAndSkipsDownstream(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: gated
version: 1.0.0
agents:
  research: {unit: a:1}
  write: {unit: b:1}
  edit: {unit: c:1}
routing:
  - from: research
    to: write
    condition: "len(research.output) > 200"
  - from: write
    to: edit
`
	spec := mustParse(t, src)
	var calls []AgentRequest
	res, err := Run(context.Background(), stubExec(map[string]string{
		"research": "too short", "write": "w", "edit": "e",
	}, &calls), RunOptions{Spec: spec, Input: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("only research should have run, got %d calls", len(calls))
	}

	// A blocked branch is a normal outcome, and the skip must propagate to
	// the agent downstream of the one that was skipped.
	byName := map[string]AgentRun{}
	for _, a := range res.Agents {
		byName[a.Name] = a
	}
	if !byName["write"].Skipped {
		t.Error("write should be skipped")
	}
	if !byName["edit"].Skipped {
		t.Error("edit should be skipped: its only upstream never ran")
	}
	if byName["write"].Error != "" {
		t.Errorf("a skip must not be reported as an error: %q", byName["write"].Error)
	}
	if !strings.Contains(byName["write"].Reason, "was false") {
		t.Errorf("skip reason should quote the condition, got %q", byName["write"].Reason)
	}
	if res.Output != "too short" {
		t.Errorf("output = %q; with every sink skipped the run's work should still surface", res.Output)
	}
}

func TestRunAppliesTransform(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: shaped
version: 1.0.0
agents:
  a: {unit: a:1}
  b: {unit: b:1}
routing:
  - from: a
    to: b
    transform: "SUMMARY: {{ upper .a.output }}"
`
	spec := mustParse(t, src)
	var calls []AgentRequest
	_, err := Run(context.Background(), stubExec(map[string]string{
		"a": "hello", "b": "done",
	}, &calls), RunOptions{Spec: spec})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls[1].Prompt != "SUMMARY: HELLO" {
		t.Errorf("transformed prompt = %q", calls[1].Prompt)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls")
	}
}

func TestRunFansIn(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: fanin
version: 1.0.0
agents:
  left: {unit: a:1}
  right: {unit: b:1}
  merge: {unit: c:1}
routing:
  - {from: left, to: merge}
  - {from: right, to: merge}
`
	spec := mustParse(t, src)
	var calls []AgentRequest
	_, err := Run(context.Background(), stubExec(map[string]string{
		"left": "L", "right": "R", "merge": "M",
	}, &calls), RunOptions{Spec: spec})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var mergePrompt string
	for _, c := range calls {
		if c.Name == "merge" {
			mergePrompt = c.Prompt
		}
	}
	// Declaration order, blank-line separated, so the fan-in is legible.
	if mergePrompt != "L\n\nR" {
		t.Errorf("merge prompt = %q, want %q", mergePrompt, "L\n\nR")
	}
}

func TestRunRetriesThenSucceeds(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: flaky
version: 1.0.0
agents:
  a:
    unit: a:1
    restart: 2
`
	spec := mustParse(t, src)
	attempts := 0
	exec := func(_ context.Context, req AgentRequest) (*AgentResult, error) {
		attempts++
		if attempts < 3 {
			return nil, fmt.Errorf("backend not ready")
		}
		return &AgentResult{Output: "ok"}, nil
	}
	res, err := Run(context.Background(), exec, RunOptions{Spec: spec})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if res.Agents[0].Attempts != 3 {
		t.Errorf("recorded attempts = %d", res.Agents[0].Attempts)
	}
}

func TestRunGivesUpAfterRestarts(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: broken
version: 1.0.0
agents:
  a:
    unit: a:1
    restart: 1
`
	spec := mustParse(t, src)
	exec := func(_ context.Context, _ AgentRequest) (*AgentResult, error) {
		return nil, fmt.Errorf("always fails")
	}
	_, err := Run(context.Background(), exec, RunOptions{Spec: spec})
	if err == nil {
		t.Fatal("expected the workflow to fail")
	}
	if !strings.Contains(err.Error(), "2 attempt") {
		t.Errorf("error should report the attempt count: %v", err)
	}
}

func TestRunRejectsEmptyOutput(t *testing.T) {
	spec := mustParse(t, linear)
	var calls []AgentRequest
	_, err := Run(context.Background(), stubExec(map[string]string{
		"first": "   ",
	}, &calls), RunOptions{Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("expected an empty-output error, got %v", err)
	}
}

func TestRunOnlyIncludesDependencies(t *testing.T) {
	spec := mustParse(t, linear)
	var calls []AgentRequest
	_, err := Run(context.Background(), stubExec(map[string]string{
		"first": "one", "second": "two", "third": "three",
	}, &calls), RunOptions{Spec: spec, Only: []string{"second"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// "second" cannot run without "first": its prompt comes from it.
	if len(calls) != 2 || calls[0].Name != "first" || calls[1].Name != "second" {
		t.Errorf("--only second should run first then second, got %v", names(calls))
	}
}

func TestRunOnlyRejectsUnknownAgent(t *testing.T) {
	spec := mustParse(t, linear)
	var calls []AgentRequest
	_, err := Run(context.Background(), stubExec(nil, &calls),
		RunOptions{Spec: spec, Only: []string{"ghost"}})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected an unknown-agent error, got %v", err)
	}
}

func TestRunPassesAgentConfiguration(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: cfg
version: 1.0.0
agents:
  a:
    unit: ghcr.io/acme/a:1
    model: ollama:phi3:3.8b
    max_tokens: 999
    hardware:
      prefer: [npu, cpu]
    env:
      TONE: technical
`
	spec := mustParse(t, src)
	var calls []AgentRequest
	if _, err := Run(context.Background(), stubExec(map[string]string{"a": "x"}, &calls),
		RunOptions{Spec: spec, DefaultMaxTokens: 256}); err != nil {
		t.Fatal(err)
	}
	got := calls[0]
	if got.Model != "ollama:phi3:3.8b" {
		t.Errorf("model = %q", got.Model)
	}
	if got.MaxTokens != 999 {
		t.Errorf("max_tokens = %d, want the agent's own", got.MaxTokens)
	}
	if len(got.Prefer) != 2 || got.Prefer[0] != "npu" {
		t.Errorf("prefer = %v", got.Prefer)
	}
	if got.Env["TONE"] != "technical" {
		t.Errorf("env = %v", got.Env)
	}
}

func TestDefaultMaxTokensApplies(t *testing.T) {
	spec := mustParse(t, `apiVersion: nexusrun.dev/v1
kind: Workflow
name: cfg
version: 1.0.0
agents:
  a: {unit: a:1}
`)
	var calls []AgentRequest
	if _, err := Run(context.Background(), stubExec(map[string]string{"a": "x"}, &calls),
		RunOptions{Spec: spec, DefaultMaxTokens: 256}); err != nil {
		t.Fatal(err)
	}
	if calls[0].MaxTokens != 256 {
		t.Errorf("max_tokens = %d, want the default", calls[0].MaxTokens)
	}
}

func TestSeveralSinksAreLabelled(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: forked
version: 1.0.0
agents:
  root: {unit: a:1}
  left: {unit: b:1}
  right: {unit: c:1}
routing:
  - {from: root, to: left}
  - {from: root, to: right}
`
	spec := mustParse(t, src)
	var calls []AgentRequest
	res, err := Run(context.Background(), stubExec(map[string]string{
		"root": "r", "left": "L", "right": "R",
	}, &calls), RunOptions{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"── left ──", "── right ──", "L", "R"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output %q should contain %q", res.Output, want)
		}
	}
}

// --- bus ------------------------------------------------------------------

func TestFileBusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.jsonl")
	bus, err := OpenBus(State{Backend: StateFile, Path: path}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := Message{ID: "msg_1", From: "a", To: "b",
		Payload: Payload{Content: "hello", Metadata: Metadata{TokensUsed: 3, Device: "gpu"}}}
	if err := bus.Publish(m); err != nil {
		t.Fatal(err)
	}
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenBus(State{Backend: StateFile, Path: path}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	msgs, err := reopened.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Payload.Content != "hello" || msgs[0].Payload.Metadata.Device != "gpu" {
		t.Fatalf("round trip lost data: %+v", msgs)
	}
}

func TestFileBusEncryption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.jsonl")
	t.Setenv(StateKeyEnv, "correct horse battery staple")

	st := State{Backend: StateFile, Path: path, Encryption: EncryptionAESGCM}
	bus, err := OpenBus(st, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(Message{ID: "m", From: "a", To: "b",
		Payload: Payload{Content: "a secret the agents shared"}}); err != nil {
		t.Fatal(err)
	}
	bus.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "a secret") {
		t.Fatal("payload is on disk in the clear")
	}

	back, err := OpenBus(st, "")
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	msgs, err := back.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Payload.Content != "a secret the agents shared" {
		t.Fatalf("decrypt failed: %+v", msgs)
	}

	// The wrong key must fail loudly, not return empty history.
	t.Setenv(StateKeyEnv, "a different key")
	wrong, err := OpenBus(st, "")
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := wrong.Messages(); err == nil {
		t.Error("reading with the wrong key should fail")
	}
}

func TestEncryptionRequiresKey(t *testing.T) {
	t.Setenv(StateKeyEnv, "")
	_, err := OpenBus(State{Backend: StateFile, Path: filepath.Join(t.TempDir(), "s.jsonl"),
		Encryption: EncryptionAESGCM}, "")
	if err == nil || !strings.Contains(err.Error(), StateKeyEnv) {
		t.Fatalf("expected an error naming %s, got %v", StateKeyEnv, err)
	}
}

func TestBusPathIsRelativeToWorkflow(t *testing.T) {
	dir := t.TempDir()
	bus, err := OpenBus(State{Backend: StateFile, Path: "./.nexus/state.jsonl"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := bus.Publish(Message{ID: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".nexus", "state.jsonl")); err != nil {
		t.Errorf("state should be written beside the workflow: %v", err)
	}
}

// --- transform ------------------------------------------------------------

func TestTransformFunctions(t *testing.T) {
	env := Env{"a": map[string]any{FieldOutput: "  Hello World  "}}
	cases := map[string]string{
		"{{ trim .a.output }}":                            "Hello World",
		"{{ upper (trim .a.output) }}":                    "HELLO WORLD",
		"{{ lower (trim .a.output) }}":                    "hello world",
		"{{ summarize 5 .a.output }}":                     "Hello…",
		"{{ default \"none\" \"\" }}":                     "none",
		"{{ replace \"World\" \"Go\" (trim .a.output) }}": "Hello Go",
	}
	for src, want := range cases {
		got, err := Transform(src, env)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", src, got, want)
		}
	}
}

func TestTransformRejectsMissingKey(t *testing.T) {
	env := Env{"a": map[string]any{FieldOutput: "x"}}
	if _, err := Transform("{{ .ghost.output }}", env); err == nil {
		t.Error("a transform referencing an agent that never ran should fail loudly")
	}
}

func names(reqs []AgentRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Name
	}
	return out
}
