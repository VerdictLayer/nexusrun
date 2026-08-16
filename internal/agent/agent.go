// Package agent runs the loop that turns a model into an agent: generate,
// execute whatever tools the model asked for, feed the results back, and
// generate again until it answers.
//
// Until this existed, a unit that declared tools got as far as reporting
// the call the model wanted to make and then failed, which was honest but
// not useful. The loop is what closes it.
//
// Two kinds of tool arrive here through one interface. A script tool is a
// program inside the unit, run under the unit's sandbox. An MCP tool lives
// in a declared server process. The model is told about both in the same
// vocabulary and cannot tell them apart, which is the point: where a
// capability comes from is the packaging's business, not the model's.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/engine"
)

// DefaultMaxTurns bounds the loop.
//
// A model that keeps calling tools without concluding is the normal
// failure mode of a weak local model with a tool it does not understand,
// and unbounded it burns the machine until someone notices. Eight is
// enough for real multi-step work and small enough to fail fast.
const DefaultMaxTurns = 8

// Executor runs one tool call and returns what the model should be told.
//
// isError marks a tool-level failure — the tool ran and refused. It is
// returned to the model rather than aborting, because "no such file" is
// something a model can recover from and usually does. A transport or
// sandbox failure is an error instead, and stops the run.
type Executor interface {
	Has(name string) bool
	Execute(ctx context.Context, call engine.ToolCall) (result string, isError bool, err error)
}

// Options configures one agent run.
type Options struct {
	Backend engine.Backend
	Request engine.Request

	// Tools offered to the model. Empty means a plain single generation.
	Tools []engine.ToolDef

	// Executors are consulted in order; the first that Has the tool runs it.
	Executors []Executor

	MaxTurns int

	// Stream receives assistant text as it arrives.
	Stream func(chunk string)

	Progress func(format string, args ...any)
}

// Step is one tool call and its outcome, kept for the transcript.
type Step struct {
	Turn    int           `json:"turn"`
	Tool    string        `json:"tool"`
	Args    string        `json:"args"`
	Result  string        `json:"result"`
	IsError bool          `json:"is_error,omitempty"`
	Took    time.Duration `json:"took"`
}

// Result is the whole exchange.
type Result struct {
	// Text is the model's final answer.
	Text string `json:"text"`

	// Messages are the turns added to the conversation, ready to append to
	// a session: assistant turns, tool-call turns, and tool results.
	Messages []engine.Message `json:"messages"`

	Steps     []Step  `json:"steps,omitempty"`
	Turns     int     `json:"turns"`
	TokensOut int     `json:"tokens_out"`
	EvalTPS   float64 `json:"eval_tokens_per_sec"`
	Backend   string  `json:"backend"`
	Device    string  `json:"device"`
}

// Run executes the agent loop.
func Run(ctx context.Context, opts Options) (*Result, error) {
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

	req := opts.Request
	req.Tools = opts.Tools
	req.Stream = opts.Stream

	// The conversation is built once and extended in place. Every turn
	// resends the whole thing, which is what a stateless completions
	// endpoint requires.
	msgs := req.Conversation()
	res := &Result{}

	for turn := 1; ; turn++ {
		req.Messages = msgs
		// Prompt and System are already folded into Messages; leaving them
		// set would make a backend that reads them append the first user
		// turn a second time.
		req.Prompt, req.System = "", ""

		gen, err := opts.Backend.Generate(ctx, req)
		if err != nil {
			return res, err
		}
		res.Turns = turn
		res.TokensOut += gen.TokensOut
		res.EvalTPS, res.Backend, res.Device = gen.EvalTPS, gen.Backend, gen.Device

		assistant := engine.Message{
			Role: engine.RoleAssistant, Content: gen.Text, ToolCalls: gen.ToolCalls,
		}
		msgs = append(msgs, assistant)
		res.Messages = append(res.Messages, assistant)

		if len(gen.ToolCalls) == 0 {
			res.Text = gen.Text
			return res, nil
		}

		if turn >= maxTurns {
			// Report what it was still trying to do; a bare "gave up" leaves
			// no way to tell a loop from a genuinely long task.
			var pending []string
			for _, c := range gen.ToolCalls {
				pending = append(pending, c.Summary())
			}
			res.Text = gen.Text
			return res, fmt.Errorf(
				"stopped after %d turns with the model still calling tools (%s) — raise --max-turns if the task really needs more",
				maxTurns, strings.Join(pending, ", "))
		}

		for _, call := range gen.ToolCalls {
			step, msg, err := runCall(ctx, opts, call, turn, logf)
			if err != nil {
				return res, err
			}
			res.Steps = append(res.Steps, step)
			msgs = append(msgs, msg)
			res.Messages = append(res.Messages, msg)
		}
	}
}

// runCall dispatches one tool call to whichever executor owns it.
func runCall(ctx context.Context, opts Options, call engine.ToolCall, turn int, logf func(string, ...any)) (Step, engine.Message, error) {
	step := Step{Turn: turn, Tool: call.Name, Args: call.Arguments}
	start := time.Now()

	// A call ID is how a result is matched to its request. Ollama omits
	// one, so synthesise a stable value rather than leaving the field empty
	// and hoping the server matches positionally.
	id := call.ID
	if id == "" {
		id = fmt.Sprintf("call_%d_%s", turn, call.Name)
	}

	reply := func(content string, isError bool) (Step, engine.Message, error) {
		step.Result, step.IsError, step.Took = content, isError, time.Since(start)
		return step, engine.Message{
			Role: engine.RoleTool, Content: content, ToolCallID: id, Name: call.Name,
		}, nil
	}

	var exec Executor
	for _, e := range opts.Executors {
		if e.Has(call.Name) {
			exec = e
			break
		}
	}
	if exec == nil {
		// The model invented a tool. That is a model error, not a runtime
		// one: tell it so and let it correct itself, which it usually does.
		var offered []string
		for _, t := range opts.Tools {
			offered = append(offered, t.Name)
		}
		logf("  ✗ %s — no such tool", call.Name)
		return reply(fmt.Sprintf(
			"Error: there is no tool named %q. Available tools: %s",
			call.Name, strings.Join(offered, ", ")), true)
	}

	logf("  → %s", call.Summary())
	out, isError, err := exec.Execute(ctx, call)
	if err != nil {
		// A failure to *run* the tool is the runtime's problem and stops
		// the agent; the model cannot do anything useful about a sandbox
		// denial or a dead server.
		return step, engine.Message{}, fmt.Errorf("tool %s: %w", call.Name, err)
	}
	status := "✓"
	if isError {
		status = "✗"
	}
	logf("  %s %s — %s", status, call.Name, oneLine(out, 100))
	return reply(out, isError)
}

// ParseArgs decodes a tool call's arguments.
//
// Models emit arguments that do not match the declared schema often enough
// that this has to be forgiving about *shape* while staying strict about
// validity: a non-object is wrapped rather than rejected, but malformed
// JSON is reported so the model can be told what it got wrong.
func ParseArgs(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		return m, nil
	}
	var any_ any
	if err := json.Unmarshal([]byte(raw), &any_); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %s", raw)
	}
	return map[string]any{"value": any_}, nil
}

func oneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
