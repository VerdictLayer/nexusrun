// Package runner executes script-type units: units whose entrypoint is a
// program rather than a chat prompt.
//
// A script unit receives the resolved model path and prompt through the
// environment, writes its result to stdout, and runs under the sandbox
// policy derived from its declared capabilities.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/sandbox"
)

// Options configures a script run.
type Options struct {
	WorkDir   string // unpacked unit directory
	ModelPath string // resolved weights, may be empty
	Prompt    string
	Device    string
	MaxTokens int

	// NoSandbox runs without confinement. It exists for platforms with
	// no implementation and for debugging; it is never the default.
	NoSandbox bool

	// HomeDir is the real user home directory, granted only when the
	// unit declares the storage capability.
	HomeDir string

	// SelfExe is the path to the nexus binary, used to re-exec a helper
	// that applies the sandbox policy before handing off to the script.
	SelfExe string

	Stdout func(chunk string)
}

// Result is the outcome of a script run.
type Result struct {
	Output   string
	ExitCode int
	Duration time.Duration
}

// Run executes a unit's script entrypoint.
func Run(ctx context.Context, m *manifest.Manifest, opts Options) (*Result, error) {
	script := m.Entrypoint.Script
	if script == "" {
		return nil, fmt.Errorf("unit %s has entrypoint.type=script but no entrypoint.script", m.Ref())
	}
	// Keep the script inside the unit; a manifest must not reach out of
	// its own directory.
	scriptPath := filepath.Join(opts.WorkDir, filepath.FromSlash(script))
	cleanWork := filepath.Clean(opts.WorkDir)
	if !strings.HasPrefix(filepath.Clean(scriptPath), cleanWork+string(os.PathSeparator)) {
		return nil, fmt.Errorf("entrypoint.script %q escapes the unit directory", script)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		return nil, fmt.Errorf("entrypoint script not found: %w", err)
	}

	argv, err := interpreterFor(scriptPath)
	if err != nil {
		return nil, err
	}

	sandboxed := false
	if !opts.NoSandbox {
		if !sandbox.Supported() {
			return nil, fmt.Errorf(
				"refusing to run script unit unsandboxed: %s\n"+
					"pass --no-sandbox to override, only for units you trust", sandbox.Describe())
		}
		if opts.SelfExe == "" {
			return nil, fmt.Errorf("internal error: sandbox helper path not set")
		}
		// The helper applies the Landlock policy to itself, then execs
		// the interpreter. Landlock is inherited and cannot be dropped.
		// Boolean flags must be a single "--flag=value" argument. Passing
		// "--flag value" leaves the value as a positional, and the helper
		// would exec that instead of the interpreter.
		helper := []string{opts.SelfExe, sandboxHelperCommand,
			"--workdir", opts.WorkDir,
			"--home", opts.HomeDir,
			fmt.Sprintf("--network=%t", hasCapability(m.Capabilities, "network")),
			fmt.Sprintf("--storage=%t", hasCapability(m.Capabilities, "storage")),
		}
		if opts.ModelPath != "" {
			helper = append(helper, "--read", opts.ModelPath)
		}
		argv = append(append(helper, "--"), argv...)
		sandboxed = true
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = opts.WorkDir
	// A deliberately small environment: a unit sees what it needs and
	// nothing else, so stray credentials in the parent env do not leak.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + opts.WorkDir,
		"NEXUS_UNIT=" + m.Ref(),
		"NEXUS_PROMPT=" + opts.Prompt,
		"NEXUS_MODEL_PATH=" + opts.ModelPath,
		"NEXUS_DEVICE=" + opts.Device,
		"NEXUS_MAX_TOKENS=" + strconv.Itoa(opts.MaxTokens),
		"NEXUS_SANDBOXED=" + strconv.FormatBool(sandboxed),
	}
	if lang := os.Getenv("LANG"); lang != "" {
		cmd.Env = append(cmd.Env, "LANG="+lang)
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = strings.NewReader(opts.Prompt)

	start := time.Now()
	err = cmd.Run()
	res := &Result{Output: out.String(), Duration: time.Since(start)}
	if opts.Stdout != nil {
		opts.Stdout(res.Output)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errorsAs(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			detail := trimTail(res.Output, 20)
			if strings.TrimSpace(detail) == "" {
				detail = "(no output) command: " + strings.Join(argv, " ")
			}
			return res, fmt.Errorf("script exited %d: %s", res.ExitCode, detail)
		}
		return res, err
	}
	return res, nil
}

// interpreterFor picks how to execute a script based on its extension,
// falling back to running it directly if it is executable.
func interpreterFor(path string) ([]string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		bin, err := lookAny("python3", "python")
		if err != nil {
			return nil, fmt.Errorf("python is required to run this unit: %w", err)
		}
		return []string{bin, path}, nil
	case ".js", ".mjs":
		bin, err := lookAny("node", "bun")
		if err != nil {
			return nil, fmt.Errorf("node is required to run this unit: %w", err)
		}
		return []string{bin, path}, nil
	case ".sh":
		bin, err := lookAny("bash", "sh")
		if err != nil {
			return nil, err
		}
		return []string{bin, path}, nil
	default:
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("don't know how to run %q: unknown extension and not executable", filepath.Base(path))
		}
		return []string{path}, nil
	}
}

func lookAny(names ...string) (string, error) {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of %s found in PATH", strings.Join(names, ", "))
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func trimTail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
