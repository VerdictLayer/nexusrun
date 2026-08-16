package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/hardware"
)

// scriptedBackend replays a fixed sequence of generations and records the
// conversation it was handed each time, which is what the loop's contract
// actually is: the whole exchange resent every turn.
type scriptedBackend struct {
	turns []*engine.Result
	seen  [][]engine.Message
	err   error
}

func (b *scriptedBackend) Name() string { return "scripted" }
func (b *scriptedBackend) Probe() engine.Capability {
	return engine.Capability{Available: true, SupportsTools: true}
}
func (b *scriptedBackend) Generate(_ context.Context, req engine.Request) (*engine.Result, error) {
	if b.err != nil {
		return nil, b.err
	}
	b.seen = append(b.seen, append([]engine.Message(nil), req.Messages...))
	if len(b.turns) == 0 {
		return nil, fmt.Errorf("scripted backend ran out of turns")
	}
	next := b.turns[0]
	b.turns = b.turns[1:]
	return next, nil
}

var _ engine.Backend = (*scriptedBackend)(nil)
var _ = hardware.ClassCPU

// fakeTools answers from a table.
type fakeTools struct {
	results map[string]string
	errors  map[string]bool
	fail    map[string]error
	calls   []engine.ToolCall
}

func (f *fakeTools) Has(name string) bool {
	_, ok := f.results[name]
	if !ok {
		_, ok = f.fail[name]
	}
	return ok
}

func (f *fakeTools) Execute(_ context.Context, call engine.ToolCall) (string, bool, error) {
	f.calls = append(f.calls, call)
	if err, ok := f.fail[call.Name]; ok {
		return "", false, err
	}
	return f.results[call.Name], f.errors[call.Name], nil
}

func TestRunWithoutToolsIsOneGeneration(t *testing.T) {
	b := &scriptedBackend{turns: []*engine.Result{{Text: "hello", TokensOut: 2}}}
	res, err := Run(context.Background(), Options{
		Backend: b,
		Request: engine.Request{System: "be brief", Prompt: "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello" || res.Turns != 1 {
		t.Errorf("text=%q turns=%d", res.Text, res.Turns)
	}
	// System and prompt must be folded into the conversation exactly once.
	first := b.seen[0]
	if len(first) != 2 || first[0].Role != engine.RoleSystem || first[1].Content != "hi" {
		t.Errorf("conversation = %+v", first)
	}
}

func TestRunExecutesToolAndFeedsResultBack(t *testing.T) {
	b := &scriptedBackend{turns: []*engine.Result{
		{ToolCalls: []engine.ToolCall{{ID: "c1", Name: "lookup", Arguments: `{"q":"x"}`}}},
		{Text: "the answer is 42", TokensOut: 5},
	}}
	tools := &fakeTools{results: map[string]string{"lookup": "42"}}

	res, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "what is it?"},
		Tools:     []engine.ToolDef{{Name: "lookup"}},
		Executors: []Executor{tools},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "the answer is 42" {
		t.Errorf("text = %q", res.Text)
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2", res.Turns)
	}
	if len(tools.calls) != 1 || tools.calls[0].Name != "lookup" {
		t.Fatalf("calls = %+v", tools.calls)
	}

	// The second generation must have seen the tool result.
	second := b.seen[1]
	var toolTurn *engine.Message
	for i := range second {
		if second[i].Role == engine.RoleTool {
			toolTurn = &second[i]
		}
	}
	if toolTurn == nil {
		t.Fatal("the tool result was not fed back to the model")
	}
	if toolTurn.Content != "42" || toolTurn.ToolCallID != "c1" || toolTurn.Name != "lookup" {
		t.Errorf("tool turn = %+v", *toolTurn)
	}

	if len(res.Steps) != 1 || res.Steps[0].Result != "42" {
		t.Errorf("steps = %+v", res.Steps)
	}
}

func TestRunSynthesisesACallIDWhenTheBackendOmitsOne(t *testing.T) {
	// Ollama does not send tool-call IDs. Leaving the field empty makes a
	// result impossible to correlate with its request.
	b := &scriptedBackend{turns: []*engine.Result{
		{ToolCalls: []engine.ToolCall{{Name: "ping", Arguments: "{}"}}},
		{Text: "done"},
	}}
	res, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "go"},
		Tools:     []engine.ToolDef{{Name: "ping"}},
		Executors: []Executor{&fakeTools{results: map[string]string{"ping": "pong"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Messages {
		if m.Role == engine.RoleTool && m.ToolCallID == "" {
			t.Error("tool result has no call ID to correlate it")
		}
	}
}

func TestRunHandlesMultipleCallsInOneTurn(t *testing.T) {
	b := &scriptedBackend{turns: []*engine.Result{
		{ToolCalls: []engine.ToolCall{
			{ID: "a", Name: "one", Arguments: "{}"},
			{ID: "b", Name: "two", Arguments: "{}"},
		}},
		{Text: "both done"},
	}}
	tools := &fakeTools{results: map[string]string{"one": "1", "two": "2"}}
	res, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "go"},
		Tools:     []engine.ToolDef{{Name: "one"}, {Name: "two"}},
		Executors: []Executor{tools},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.calls) != 2 {
		t.Errorf("calls = %d, want both executed", len(tools.calls))
	}
	if len(res.Steps) != 2 {
		t.Errorf("steps = %d", len(res.Steps))
	}
}

func TestToolErrorGoesBackToTheModelRatherThanAborting(t *testing.T) {
	// "no such file" is something a model recovers from; it must not end
	// the run.
	b := &scriptedBackend{turns: []*engine.Result{
		{ToolCalls: []engine.ToolCall{{ID: "c", Name: "read", Arguments: `{"p":"/nope"}`}}},
		{Text: "that file does not exist"},
	}}
	tools := &fakeTools{
		results: map[string]string{"read": "Error: no such file"},
		errors:  map[string]bool{"read": true},
	}
	res, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "read /nope"},
		Tools:     []engine.ToolDef{{Name: "read"}},
		Executors: []Executor{tools},
	})
	if err != nil {
		t.Fatalf("a tool-level error should not abort the run: %v", err)
	}
	if res.Text != "that file does not exist" {
		t.Errorf("text = %q", res.Text)
	}
	if !res.Steps[0].IsError {
		t.Error("the step should be marked as an error")
	}
}

func TestExecutorFailureAbortsTheRun(t *testing.T) {
	// A sandbox denial or a dead server is the runtime's problem; the
	// model can do nothing useful with it.
	b := &scriptedBackend{turns: []*engine.Result{
		{ToolCalls: []engine.ToolCall{{ID: "c", Name: "broken", Arguments: "{}"}}},
	}}
	tools := &fakeTools{fail: map[string]error{"broken": fmt.Errorf("server died")}}
	_, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "go"},
		Tools:     []engine.ToolDef{{Name: "broken"}},
		Executors: []Executor{tools},
	})
	if err == nil || !strings.Contains(err.Error(), "server died") {
		t.Fatalf("expected the run to abort, got %v", err)
	}
}

func TestUnknownToolIsReportedToTheModel(t *testing.T) {
	b := &scriptedBackend{turns: []*engine.Result{
		{ToolCalls: []engine.ToolCall{{ID: "c", Name: "invented", Arguments: "{}"}}},
		{Text: "sorry, I will use the real one"},
	}}
	res, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "go"},
		Tools:     []engine.ToolDef{{Name: "real_tool"}},
		Executors: []Executor{&fakeTools{results: map[string]string{"real_tool": "ok"}}},
	})
	if err != nil {
		t.Fatalf("a hallucinated tool name should not abort the run: %v", err)
	}
	// The correction must name what actually exists, or the model has
	// nothing to correct toward.
	var toolMsg string
	for _, m := range res.Messages {
		if m.Role == engine.RoleTool {
			toolMsg = m.Content
		}
	}
	for _, want := range []string{"invented", "real_tool"} {
		if !strings.Contains(toolMsg, want) {
			t.Errorf("correction %q should mention %q", toolMsg, want)
		}
	}
}

func TestMaxTurnsStopsARunawayLoop(t *testing.T) {
	var turns []*engine.Result
	for i := 0; i < 20; i++ {
		turns = append(turns, &engine.Result{
			ToolCalls: []engine.ToolCall{{ID: "c", Name: "spin", Arguments: "{}"}},
		})
	}
	b := &scriptedBackend{turns: turns}
	_, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "go"},
		Tools:     []engine.ToolDef{{Name: "spin"}},
		Executors: []Executor{&fakeTools{results: map[string]string{"spin": "again"}}},
		MaxTurns:  3,
	})
	if err == nil {
		t.Fatal("expected the loop to be bounded")
	}
	// The error must say what it was still doing, or a loop and a long
	// task look identical.
	for _, want := range []string{"3 turns", "spin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestExecutorsAreConsultedInOrder(t *testing.T) {
	first := &fakeTools{results: map[string]string{"shared": "from-first"}}
	second := &fakeTools{results: map[string]string{"shared": "from-second"}}
	b := &scriptedBackend{turns: []*engine.Result{
		{ToolCalls: []engine.ToolCall{{ID: "c", Name: "shared", Arguments: "{}"}}},
		{Text: "ok"},
	}}
	res, err := Run(context.Background(), Options{
		Backend:   b,
		Request:   engine.Request{Prompt: "go"},
		Tools:     []engine.ToolDef{{Name: "shared"}},
		Executors: []Executor{first, second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[0].Result != "from-first" {
		t.Errorf("result = %q, want the first executor's", res.Steps[0].Result)
	}
	if len(second.calls) != 0 {
		t.Error("the second executor should not have been consulted")
	}
}

func TestParseArgs(t *testing.T) {
	cases := map[string]map[string]any{
		`{"a":1}`: {"a": float64(1)},
		``:        {},
		`null`:    {},
		`{}`:      {},
	}
	for raw, want := range cases {
		got, err := ParseArgs(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%q = %v, want %v", raw, got, want)
		}
	}

	// A non-object is wrapped rather than rejected: models emit bare
	// values often enough that failing would be the more common outcome.
	got, err := ParseArgs(`"just a string"`)
	if err != nil {
		t.Fatal(err)
	}
	if got["value"] != "just a string" {
		t.Errorf("wrapped = %v", got)
	}

	if _, err := ParseArgs(`{not json`); err == nil {
		t.Error("malformed JSON should be reported")
	}
}
