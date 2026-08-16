package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AgentRequest is one agent's execution, fully resolved: the workflow has
// already decided the prompt, the model, and where it should run.
type AgentRequest struct {
	Name      string
	Unit      string
	Model     string // override; empty means the unit's own
	Prefer    []string
	Env       map[string]string
	MaxTokens int
	Prompt    string

	MaxMemoryMB   int
	MaxCPUPercent int
}

// AgentResult is what an agent produced.
type AgentResult struct {
	Output    string
	TokensOut int
	TokPerSec float64
	Backend   string
	Device    string
}

// ExecFunc runs one agent. Injecting it keeps this package free of any
// opinion about how a unit executes — daemon, direct, or a test double —
// and makes the whole orchestration testable without a model.
type ExecFunc func(ctx context.Context, req AgentRequest) (*AgentResult, error)

// RunOptions configures a workflow execution.
type RunOptions struct {
	Spec *Spec

	// Input is delivered to every source agent (one nothing routes into).
	Input string

	// BaseDir resolves relative state paths; normally the directory the
	// workflow file was read from.
	BaseDir string

	// Bus, when nil, is opened from the spec's shared_state.
	Bus Bus

	// DefaultMaxTokens applies to agents that declare none.
	DefaultMaxTokens int

	// Only, when non-empty, restricts execution to these agents and their
	// dependencies — the inner loop when one stage is being worked on.
	Only []string

	Progress func(format string, args ...any)
}

// AgentRun is one agent's outcome within a workflow run.
type AgentRun struct {
	Name     string        `json:"name"`
	Unit     string        `json:"unit"`
	Prompt   string        `json:"prompt,omitempty"`
	Output   string        `json:"output,omitempty"`
	Tokens   int           `json:"tokens,omitempty"`
	TokPS    float64       `json:"tokens_per_sec,omitempty"`
	Backend  string        `json:"backend,omitempty"`
	Device   string        `json:"device,omitempty"`
	Took     time.Duration `json:"took"`
	Attempts int           `json:"attempts"`

	// Skipped is set when no inbound route fired, which is a normal
	// outcome for a conditional branch and must not read as a failure.
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Result is the whole workflow's outcome.
type Result struct {
	Workflow string            `json:"workflow"`
	Agents   []AgentRun        `json:"agents"`
	Messages []Message         `json:"messages"`
	Outputs  map[string]string `json:"outputs"`
	Output   string            `json:"output"`
	Took     time.Duration     `json:"took"`
}

// Run executes a workflow in topological order.
//
// Execution is sequential even where the graph would allow parallelism.
// That is a deliberate choice for the machines NexusRun targets: two local
// models resident at once is the difference between a workflow that runs
// on a 4 GB Pi and one that is killed by the OOM reaper, and inference is
// already saturating the accelerator, so concurrency buys throughput only
// on hardware that is not the point of this project.
func Run(ctx context.Context, exec ExecFunc, opts RunOptions) (*Result, error) {
	if opts.Spec == nil {
		return nil, fmt.Errorf("no workflow given")
	}
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}
	spec := opts.Spec

	order, err := spec.Order()
	if err != nil {
		return nil, err
	}
	if len(opts.Only) > 0 {
		if order, err = restrict(spec, order, opts.Only); err != nil {
			return nil, err
		}
	}

	bus := opts.Bus
	if bus == nil {
		if bus, err = OpenBus(spec.SharedState, opts.BaseDir); err != nil {
			return nil, err
		}
		defer bus.Close()
	}

	start := time.Now()
	res := &Result{Workflow: spec.Ref(), Outputs: map[string]string{}}

	// env is the value namespace conditions and transforms read. It grows
	// as agents finish, which is why conditions can only ever reference
	// upstream agents: nothing downstream exists yet.
	env := Env{
		CondAlways: true,
		"never":    false,
		"input":    opts.Input,
	}

	for _, name := range order {
		agent := spec.Agents[name]
		run := AgentRun{Name: name, Unit: agent.Unit}

		prompt, delivered, skipReason, err := spec.gather(name, opts.Input, env)
		if err != nil {
			run.Error = err.Error()
			res.Agents = append(res.Agents, run)
			res.Took = time.Since(start)
			return res, fmt.Errorf("agent %q: %w", name, err)
		}
		if skipReason != "" {
			run.Skipped, run.Reason = true, skipReason
			logf("skip %s — %s", name, skipReason)
			res.Agents = append(res.Agents, run)
			continue
		}

		for _, m := range delivered {
			if err := bus.Publish(m); err != nil {
				logf("warning: could not record message %s → %s: %v", m.From, m.To, err)
			}
			res.Messages = append(res.Messages, m)
		}

		maxTok := agent.MaxTokens
		if maxTok == 0 {
			maxTok = opts.DefaultMaxTokens
		}
		run.Prompt = prompt

		logf("▶ %s (%s)", name, agent.Unit)
		out, attempts, took, err := attempt(ctx, exec, AgentRequest{
			Name:          name,
			Unit:          agent.Unit,
			Model:         agent.Model,
			Prefer:        agent.Hardware.Prefer,
			Env:           agent.Env,
			MaxTokens:     maxTok,
			Prompt:        prompt,
			MaxMemoryMB:   agent.MaxMemoryMB,
			MaxCPUPercent: agent.MaxCPUPercent,
		}, agent.Restart, logf)

		run.Attempts, run.Took = attempts, took
		if err != nil {
			run.Error = err.Error()
			res.Agents = append(res.Agents, run)
			res.Took = time.Since(start)
			return res, fmt.Errorf("agent %q failed after %d attempt(s): %w", name, attempts, err)
		}
		if strings.TrimSpace(out.Output) == "" {
			run.Error = "produced no output"
			res.Agents = append(res.Agents, run)
			res.Took = time.Since(start)
			return res, fmt.Errorf("agent %q produced no output; nothing to route onward", name)
		}

		run.Output, run.Tokens, run.TokPS = out.Output, out.TokensOut, out.TokPerSec
		run.Backend, run.Device = out.Backend, out.Device
		res.Agents = append(res.Agents, run)
		res.Outputs[name] = out.Output

		env[name] = map[string]any{
			FieldOutput:    out.Output,
			FieldTokens:    float64(out.TokensOut),
			FieldTokPerSec: out.TokPerSec,
			FieldTookMS:    float64(took.Milliseconds()),
			FieldDevice:    out.Device,
			FieldBackend:   out.Backend,
			FieldOK:        true,
		}
		logf("  %d chars · %d tokens · %.1f tok/s · %s", len(out.Output), out.TokensOut, out.TokPerSec, took.Round(time.Millisecond))
	}

	res.Output = finalOutput(spec, res.Outputs)
	res.Took = time.Since(start)
	return res, nil
}

// gather builds an agent's prompt from the routes that fired.
//
// It returns the prompt, the messages that produced it, and — when no
// inbound route fired — the reason the agent is being skipped. A source
// agent (nothing routes into it) always runs, on the workflow's input.
func (s *Spec) gather(name, input string, env Env) (prompt string, delivered []Message, skip string, err error) {
	inbound := s.Inbound(name)
	if len(inbound) == 0 {
		return input, nil, "", nil
	}

	var parts []string
	var blocked []string
	for _, r := range inbound {
		// An upstream agent that was itself skipped has no value in env,
		// so its routes cannot fire — the skip propagates down the branch.
		if _, ran := env[r.From]; !ran {
			blocked = append(blocked, fmt.Sprintf("%s did not run", r.From))
			continue
		}
		ok, evalErr := Eval(r.Condition, env)
		if evalErr != nil {
			return "", nil, "", fmt.Errorf("route %s → %s: condition %q: %w", r.From, r.To, r.Condition, evalErr)
		}
		if !ok {
			blocked = append(blocked, fmt.Sprintf("%s → %s: %s was false", r.From, r.To, r.Condition))
			continue
		}

		content, _ := env[r.From].(map[string]any)[FieldOutput].(string)
		transformed := false
		if r.Transform != "" {
			out, terr := Transform(r.Transform, env)
			if terr != nil {
				return "", nil, "", fmt.Errorf("route %s → %s: %w", r.From, r.To, terr)
			}
			content, transformed = out, true
		}
		parts = append(parts, content)

		meta := Metadata{}
		if v, ok := env[r.From].(map[string]any); ok {
			meta.TokensUsed = int(toFloat(v[FieldTokens]))
			meta.TokPerSec = toFloat(v[FieldTokPerSec])
			meta.Runtime, _ = v[FieldBackend].(string)
			meta.Device, _ = v[FieldDevice].(string)
		}
		delivered = append(delivered, Message{
			ID:        newMessageID(),
			From:      r.From,
			To:        r.To,
			Timestamp: time.Now().UTC(),
			Payload:   Payload{Content: content, Metadata: meta},
			RoutingContext: RoutingContext{
				Condition:        r.Condition,
				ConditionMatched: true,
				TransformApplied: transformed,
			},
		})
	}

	if len(parts) == 0 {
		return "", nil, strings.Join(blocked, "; "), nil
	}
	// Several inbound routes concatenate in declaration order, separated by
	// a blank line. A fan-in that silently interleaved would be far harder
	// to debug than one that is visibly two blocks of text.
	return strings.Join(parts, "\n\n"), delivered, "", nil
}

// attempt runs an agent, retrying up to restart times. Retries exist for
// the crash case — a backend that died, a server that was still starting —
// so the delay backs off rather than hammering a process that needs time.
func attempt(ctx context.Context, exec ExecFunc, req AgentRequest, restart int, logf func(string, ...any)) (*AgentResult, int, time.Duration, error) {
	start := time.Now()
	var lastErr error
	for i := 0; i <= restart; i++ {
		if i > 0 {
			delay := time.Duration(i) * time.Second
			logf("  retry %d/%d in %s after: %v", i, restart, delay, lastErr)
			select {
			case <-ctx.Done():
				return nil, i, time.Since(start), ctx.Err()
			case <-time.After(delay):
			}
		}
		out, err := exec(ctx, req)
		if err == nil {
			return out, i + 1, time.Since(start), nil
		}
		lastErr = err
		// A cancelled context will not succeed on retry.
		if ctx.Err() != nil {
			return nil, i + 1, time.Since(start), err
		}
	}
	return nil, restart + 1, time.Since(start), lastErr
}

// finalOutput is what the workflow as a whole produced: the sink agents'
// outputs. One sink is the common case and returns bare text; several are
// labelled, because an unlabelled concatenation of two branches is not
// something a caller can take apart again.
func finalOutput(s *Spec, outputs map[string]string) string {
	var produced []string
	for _, n := range s.Sinks() {
		if _, ok := outputs[n]; ok {
			produced = append(produced, n)
		}
	}
	if len(produced) == 0 {
		// Every sink was skipped; fall back to whatever did run, so a
		// conditional workflow still returns its work.
		for n := range outputs {
			produced = append(produced, n)
		}
		sort.Strings(produced)
	}
	if len(produced) == 1 {
		return outputs[produced[0]]
	}
	var b strings.Builder
	for i, n := range produced {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "── %s ──\n%s", n, strings.TrimSpace(outputs[n]))
	}
	return b.String()
}

// restrict narrows the order to the named agents plus everything they
// depend on, keeping topological order. Running a stage without its
// upstream would feed it the workflow input instead of its real prompt.
func restrict(s *Spec, order []string, only []string) ([]string, error) {
	deps := s.Edges()
	want := map[string]bool{}

	var add func(string) error
	add = func(n string) error {
		if want[n] {
			return nil
		}
		if _, ok := s.Agents[n]; !ok {
			return fmt.Errorf("--only names %q, which is not an agent in this workflow", n)
		}
		want[n] = true
		for _, d := range deps[n] {
			if err := add(d); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range only {
		if err := add(n); err != nil {
			return nil, err
		}
	}

	var out []string
	for _, n := range order {
		if want[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

func toFloat(v any) float64 {
	f, _ := asNumber(v)
	return f
}

func newMessageID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "msg_" + fmt.Sprint(time.Now().UnixNano())
	}
	return "msg_" + hex.EncodeToString(b[:])
}
