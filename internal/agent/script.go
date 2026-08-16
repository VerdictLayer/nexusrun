package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/runner"
	"github.com/verdictlayer/nexusrun/internal/sandbox"
)

// ScriptTools executes the tools a unit ships with.
//
// The contract is deliberately plain: the tool is handed the call's
// arguments as JSON on stdin and whatever it writes to stdout is what the
// model is told. No schema is imposed on the reply, because a tool that
// returns a sentence is as useful to a model as one that returns JSON, and
// forcing a shape would rule out the simplest possible tool.
type ScriptTools struct {
	tools map[string]manifest.Tool

	workDir   string
	homeDir   string
	selfExe   string
	noSandbox bool
	env       []string

	// unitCaps is what the unit as a whole declared. A tool's own
	// capability list can only narrow it, which validation already enforces
	// — this is where that narrowing is actually applied.
	unitCaps []string

	timeout time.Duration
}

// ScriptOptions configures the script executor.
type ScriptOptions struct {
	WorkDir   string
	HomeDir   string
	SelfExe   string
	NoSandbox bool

	// Env is the unit's resolved secrets and config.
	Env []string

	// Timeout bounds one tool call. Default 30s.
	Timeout time.Duration
}

// NewScriptTools builds an executor for a unit's declared tools.
func NewScriptTools(m *manifest.Manifest, opts ScriptOptions) *ScriptTools {
	st := &ScriptTools{
		tools: map[string]manifest.Tool{}, workDir: opts.WorkDir,
		homeDir: opts.HomeDir, selfExe: opts.SelfExe, noSandbox: opts.NoSandbox,
		env: opts.Env, unitCaps: m.Capabilities, timeout: opts.Timeout,
	}
	if st.timeout <= 0 {
		st.timeout = 30 * time.Second
	}
	for _, t := range m.Tools {
		st.tools[t.Name] = t
	}
	return st
}

// Has reports whether this executor owns the tool.
func (st *ScriptTools) Has(name string) bool {
	_, ok := st.tools[name]
	return ok
}

// Count is how many tools are available.
func (st *ScriptTools) Count() int { return len(st.tools) }

// Execute runs one tool.
func (st *ScriptTools) Execute(ctx context.Context, call engine.ToolCall) (string, bool, error) {
	t, ok := st.tools[call.Name]
	if !ok {
		return "", false, fmt.Errorf("no script tool named %q", call.Name)
	}
	if t.Exec.Type != manifest.ToolExecScript || len(t.Exec.Command) == 0 {
		return "", false, fmt.Errorf("tool %s has no runnable command", call.Name)
	}

	// The arguments are validated as JSON before the tool sees them. A
	// tool handed malformed JSON on stdin fails in its own parser with its
	// own message, which reads like the tool is broken.
	args, err := ParseArgs(call.Arguments)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true, nil
	}
	payload, err := marshalArgs(args)
	if err != nil {
		return "", false, err
	}

	argv := append([]string{}, t.Exec.Command...)
	// A relative command resolves inside the unit; a tool must not reach
	// out of its own directory to find its program.
	if !filepath.IsAbs(argv[0]) {
		candidate := filepath.Join(st.workDir, filepath.FromSlash(argv[0]))
		if _, serr := os.Stat(candidate); serr == nil {
			clean := filepath.Clean(candidate)
			if !strings.HasPrefix(clean, filepath.Clean(st.workDir)+string(os.PathSeparator)) {
				return "", false, fmt.Errorf("tool %s: command %q escapes the unit directory", call.Name, argv[0])
			}
			argv[0] = clean
		}
	}

	// A tool's capabilities narrow the unit's, never widen them.
	caps := t.Capabilities
	if caps == nil {
		caps = st.unitCaps
	}
	network := contains(caps, "network") && contains(st.unitCaps, "network")
	storage := contains(caps, "storage") && contains(st.unitCaps, "storage")

	if !st.noSandbox {
		if !sandbox.Supported() {
			return "", false, fmt.Errorf(
				"refusing to run tool %s unsandboxed: %s\n"+
					"pass --no-sandbox to override, only for units you trust", call.Name, sandbox.Describe())
		}
		if st.selfExe == "" {
			return "", false, fmt.Errorf("internal error: sandbox helper path not set")
		}
		helper := []string{st.selfExe, runner.HelperCommandName(),
			"--workdir", st.workDir,
			"--home", st.homeDir,
			fmt.Sprintf("--network=%t", network),
			fmt.Sprintf("--storage=%t", storage),
		}
		argv = append(append(helper, "--"), argv...)
	}

	callCtx, cancel := context.WithTimeout(ctx, st.timeout)
	defer cancel()

	cmd := exec.CommandContext(callCtx, argv[0], argv[1:]...)
	cmd.Dir = st.workDir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + st.workDir,
		"NEXUS_TOOL=" + call.Name,
		"NEXUS_SANDBOXED=" + fmt.Sprintf("%t", !st.noSandbox),
	}, st.env...)
	cmd.Stdin = bytes.NewReader(payload)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if callCtx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("Error: tool %s timed out after %s", call.Name, st.timeout), true, nil
		}
		// A non-zero exit is the tool saying no, which the model can act
		// on. Its stderr is the useful part and is what gets returned.
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return "Error: " + detail, true, nil
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		// A tool that succeeds silently still has to say something, or the
		// model sees an empty turn and usually repeats the call.
		out = "(the tool produced no output)"
	}
	return out, false, nil
}

func marshalArgs(args map[string]any) ([]byte, error) {
	return jsonMarshal(args)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
