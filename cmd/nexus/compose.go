package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/verdictlayer/nexusrun/internal/compose"
	"github.com/verdictlayer/nexusrun/internal/daemon"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
	"github.com/verdictlayer/nexusrun/internal/workflow"
)

// newComposeCmd assembles the compose command tree.
//
// The bare positional form (`nexus compose a:1 b:1`) is the original
// sequential pipeline and still works: it is the two-minute version that
// needs no file. The subcommands operate on a nexus-compose.yaml, which is
// the version you can version-control, validate in CI, and ship.
func newComposeCmd() *cobra.Command {
	var input string
	var maxTokens int
	var showStages bool

	cmd := &cobra.Command{
		Use:   "compose <unit> <unit> [unit...]",
		Short: "Run multi-agent workflows",
		Long: `Compose runs several units together.

Given unit references, it pipes each output into the next — the quickest
possible composition, with nothing to write down:

  nexus compose summarizer:0.1.0 translator:0.1.0 --input "$(cat report.txt)"

Given a nexus-compose.yaml, it runs a declared workflow: a dependency
graph with routing conditions, payload transforms, and a shared state bus.

  nexus compose validate            # check the file, run nothing
  nexus compose up                  # run it
  nexus compose build -t ghcr.io/acme/pipeline:v1   # package it`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			runStage := func(ctx context.Context, ref, prompt string) (string, float64, error) {
				if res := tryDaemon(ctx, daemon.RunRequest{
					Unit: ref, Prompt: prompt, MaxTokens: maxTokens,
				}); res != nil {
					return res.Text, res.EvalTPS, nil
				}
				return runUnitDirect(ctx, s, ref, prompt, maxTokens)
			}

			res, err := compose.Run(ctx, runStage, compose.Options{
				Units: args, Input: input, Progress: logf,
			})
			if res != nil && showStages {
				fmt.Println()
				for i, st := range res.Stages {
					fmt.Printf("── stage %d: %s ──\n%s\n\n", i+1, st.Unit, strings.TrimSpace(st.Output))
				}
			}
			if err != nil {
				return err
			}
			if !showStages {
				fmt.Println(res.Output)
			}
			logf("pipeline finished in %s", res.Took.Round(time.Millisecond))
			return nil
		},
	}
	cmd.Flags().StringVarP(&input, "input", "i", "", "input for the first unit")
	cmd.Flags().IntVarP(&maxTokens, "max-tokens", "n", 256, "max tokens per stage")
	cmd.Flags().BoolVar(&showStages, "stages", false, "print every intermediate output")

	cmd.AddCommand(
		newComposeValidateCmd(),
		newComposeUpCmd(),
		newComposeDownCmd(),
		newComposeLogsCmd(),
		newComposeListCmd(),
		newComposeBuildCmd(),
		newComposePushCmd(),
		newComposePullCmd(),
		newComposeInitCmd(),
	)
	return cmd
}

// composeFileFlag adds the shared -f/--file flag.
func composeFileFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVarP(target, "file", "f", workflow.FileName, "workflow file")
}

// loadWorkflow resolves the -f argument, which may name a file, a
// directory containing one, or a locally stored workflow reference.
func loadWorkflow(ctx context.Context, s *store.Store, ref string) (*workflow.Spec, string, error) {
	if _, err := os.Stat(ref); err == nil {
		spec, err := workflow.Load(ref)
		if err != nil {
			return nil, "", err
		}
		abs, _ := filepath.Abs(ref)
		if info, serr := os.Stat(abs); serr == nil && !info.IsDir() {
			abs = filepath.Dir(abs)
		}
		return spec, abs, nil
	}
	spec, err := workflow.Resolve(ctx, s, ref)
	if err != nil {
		return nil, "", err
	}
	// A workflow pulled from a registry has no directory of its own, so a
	// relative state path resolves against the working directory.
	cwd, _ := os.Getwd()
	return spec, cwd, nil
}

// --- validate -------------------------------------------------------------

func newComposeValidateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate [file]",
		Short: "Check a workflow without running it",
		Long: `Validate parses the workflow and checks everything that can be checked
without executing a model: unknown fields, agents that do not exist,
dependency cycles, routing conditions that do not parse or that name a
field an agent does not have, and transforms that will not render.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := file
			if len(args) == 1 {
				path = args[0]
			}
			s, err := store.Open()
			if err != nil {
				return err
			}
			// A file, a directory, or a workflow already in the store: the
			// last is how you inspect one you just pulled.
			spec, _, err := loadWorkflow(cmd.Context(), s, path)
			if err != nil {
				return err
			}
			order, err := spec.Order()
			if err != nil {
				return err
			}
			fmt.Printf("%s is valid.\n\n", path)
			fmt.Printf("Workflow:  %s\n", spec.Ref())
			if spec.Description != "" {
				fmt.Printf("           %s\n", spec.Description)
			}
			fmt.Printf("State:     %s", spec.SharedState.Backend)
			if spec.SharedState.Path != "" {
				fmt.Printf(" at %s", spec.SharedState.Path)
			}
			if spec.SharedState.Encryption != "" {
				fmt.Printf(" (%s)", spec.SharedState.Encryption)
			}
			fmt.Printf("\nIsolation: %s\n\nExecution order:\n", spec.Network.Isolation)
			for i, name := range order {
				a := spec.Agents[name]
				fmt.Printf("  %d. %-16s %s", i+1, name, a.Unit)
				if a.Model != "" {
					fmt.Printf(" (model: %s)", a.Model)
				}
				fmt.Println()
				for _, r := range spec.Inbound(name) {
					cond := ""
					if r.Condition != workflow.CondAlways {
						cond = "  if " + r.Condition
					}
					fmt.Printf("       ← %s%s\n", r.From, cond)
				}
			}
			return nil
		},
	}
	composeFileFlag(cmd, &file)
	return cmd
}

// --- up -------------------------------------------------------------------

func newComposeUpCmd() *cobra.Command {
	var file, input, inputFile string
	var maxTokens int
	var detach, jsonOut, showStages bool
	var only []string

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run a workflow",
		Long: `Up executes a workflow: each agent runs in dependency order, receiving
whatever its inbound routes delivered.

Agents run one at a time even where the graph would allow parallelism.
On the machines NexusRun targets, two resident models is the difference
between a workflow that runs and one the OOM killer stops.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			spec, baseDir, err := loadWorkflow(ctx, s, file)
			if err != nil {
				return err
			}

			if inputFile != "" {
				data, rerr := os.ReadFile(inputFile)
				if rerr != nil {
					return rerr
				}
				input = string(data)
			}

			if detach {
				return detachWorkflow(s, spec, file, input, maxTokens)
			}

			// A detached run is stopped with SIGTERM; honouring it here is
			// what makes `compose down` a clean shutdown rather than a kill.
			ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer cancel()

			res, runErr := workflow.Run(ctx, workflowExec(s), workflow.RunOptions{
				Spec:             spec,
				Input:            input,
				BaseDir:          baseDir,
				DefaultMaxTokens: maxTokens,
				Only:             only,
				Progress:         logf,
			})

			if jsonOut && res != nil {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return err
				}
				return runErr
			}
			if res != nil && showStages {
				fmt.Println()
				for _, a := range res.Agents {
					if a.Skipped {
						fmt.Printf("── %s ── skipped: %s\n\n", a.Name, a.Reason)
						continue
					}
					fmt.Printf("── %s (%s) ──\n%s\n\n", a.Name, a.Unit, strings.TrimSpace(a.Output))
				}
			}
			if runErr != nil {
				return runErr
			}
			if !showStages {
				fmt.Println(res.Output)
			}
			logf("workflow %s finished in %s", spec.Ref(), res.Took.Round(time.Millisecond))
			return nil
		},
	}
	composeFileFlag(cmd, &file)
	cmd.Flags().StringVarP(&input, "input", "i", "", "input delivered to every source agent")
	cmd.Flags().StringVar(&inputFile, "input-file", "", "read the input from a file")
	cmd.Flags().IntVarP(&maxTokens, "max-tokens", "n", 256, "default max tokens for agents that declare none")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run in the background")
	cmd.Flags().BoolVar(&showStages, "stages", false, "print every agent's output")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().StringArrayVar(&only, "only", nil, "run just these agents and their dependencies (repeatable)")
	return cmd
}

// workflowExec adapts a workflow agent to the existing execution paths:
// the warm daemon when one is listening, direct execution otherwise.
func workflowExec(s *store.Store) workflow.ExecFunc {
	return func(ctx context.Context, req workflow.AgentRequest) (*workflow.AgentResult, error) {
		// The daemon has no model-override or hardware-preference path, so
		// an agent that asked for either is executed directly rather than
		// silently answered by a different model than it requested.
		if req.Model == "" && len(req.Prefer) == 0 && len(req.Env) == 0 {
			if res := tryDaemon(ctx, daemon.RunRequest{
				Unit: req.Unit, Prompt: req.Prompt, MaxTokens: req.MaxTokens,
			}); res != nil {
				return &workflow.AgentResult{
					Output: res.Text, TokensOut: res.TokensOut, TokPerSec: res.EvalTPS,
					Backend: res.Backend, Device: res.Device,
				}, nil
			}
		}
		return runAgentDirect(ctx, s, req)
	}
}

// detachWorkflow re-executes this binary in the background and records the
// session so `compose down` can find it.
func detachWorkflow(s *store.Store, spec *workflow.Spec, file, input string, maxTokens int) error {
	if spec.SharedState.Backend != workflow.StateFile {
		// A detached run with an in-process bus would leave nothing to read.
		return fmt.Errorf(
			"a detached workflow needs a durable state bus so `compose logs` has something to read — set shared_state.backend to %q in %s",
			workflow.StateFile, file)
	}
	if existing, err := workflow.LoadSession(s, spec.Name); err == nil && workflow.Alive(existing.PID) {
		return fmt.Errorf("%s is already running (pid %d) — stop it with `%s compose down %s`",
			spec.Name, existing.PID, binName, spec.Name)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(file)
	args := []string{"compose", "up", "-f", abs, "-n", fmt.Sprint(maxTokens)}
	if input != "" {
		args = append(args, "--input", input)
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = filepath.Dir(abs)
	cmd.Stdin = nil

	logPath := filepath.Join(s.LogsDir(), "workflow-"+spec.Name+".log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd.Stdout, cmd.Stderr = lf, lf
	workflow.Detach(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}
	// The child is on its own from here; not reaping it is what detaching
	// means, and the session file is how it is found again.
	go func() { _ = cmd.Wait() }()

	statePath := spec.SharedState.Path
	if !filepath.IsAbs(statePath) {
		statePath = filepath.Join(filepath.Dir(abs), statePath)
	}
	sess := &workflow.Session{
		Name: spec.Name, Ref: spec.Ref(), File: abs,
		PID: cmd.Process.Pid, StatePath: statePath, Started: time.Now(),
	}
	if err := workflow.SaveSession(s, sess); err != nil {
		return err
	}
	fmt.Printf("Started %s in the background (pid %d)\n  logs:  %s compose logs %s\n  stop:  %s compose down %s\n",
		spec.Ref(), sess.PID, binName, spec.Name, binName, spec.Name)
	return nil
}

// --- down / ls ------------------------------------------------------------

func newComposeDownCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "down [name]",
		Short: "Stop a detached workflow",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			} else {
				spec, err := workflow.Load(file)
				if err != nil {
					return fmt.Errorf("no workflow name given and %s could not be read: %w", file, err)
				}
				name = spec.Name
			}
			sess, err := workflow.LoadSession(s, name)
			if err != nil {
				return err
			}
			if !workflow.Alive(sess.PID) {
				_ = workflow.RemoveSession(s, name)
				fmt.Printf("%s was not running; cleared its record.\n", name)
				return nil
			}
			if err := workflow.Stop(sess.PID); err != nil {
				return fmt.Errorf("stop %s (pid %d): %w", name, sess.PID, err)
			}
			// Give the run a moment to flush and exit before declaring it
			// stopped, so `compose ls` immediately afterwards agrees.
			for i := 0; i < 50 && workflow.Alive(sess.PID); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			_ = workflow.RemoveSession(s, name)
			fmt.Printf("Stopped %s.\n", name)
			return nil
		},
	}
	composeFileFlag(cmd, &file)
	return cmd
}

func newComposeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "ps"},
		Short:   "List detached workflows",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			sessions, err := workflow.Sessions(s)
			if err != nil {
				return err
			}
			if len(sessions) == 0 {
				fmt.Printf("No workflows running. Start one with `%s compose up -d`.\n", binName)
				return nil
			}
			fmt.Printf("%-24s %-8s %-10s %s\n", "WORKFLOW", "PID", "UPTIME", "FILE")
			for _, sess := range sessions {
				fmt.Printf("%-24s %-8d %-10s %s\n", sess.Ref, sess.PID,
					time.Since(sess.Started).Round(time.Second), sess.File)
			}
			return nil
		},
	}
}

// --- logs -----------------------------------------------------------------

func newComposeLogsCmd() *cobra.Command {
	var file string
	var follow, jsonOut bool
	cmd := &cobra.Command{
		Use:   "logs [name]",
		Short: "Show the messages agents sent each other",
		Long: `Logs reads the workflow's shared state bus: every payload that crossed
between agents, which condition let it through, and whether it was
transformed on the way.

This needs a durable bus (shared_state.backend: file). The memory backend
never leaves the process that ran it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}

			var st workflow.State
			var baseDir string
			if len(args) == 1 {
				sess, serr := workflow.LoadSession(s, args[0])
				if serr != nil {
					return serr
				}
				st = workflow.State{Backend: workflow.StateFile, Path: sess.StatePath}
				spec, _, lerr := loadWorkflow(ctx, s, sess.File)
				if lerr == nil {
					st.Encryption = spec.SharedState.Encryption
				}
			} else {
				spec, dir, lerr := loadWorkflow(ctx, s, file)
				if lerr != nil {
					return lerr
				}
				st, baseDir = spec.SharedState, dir
			}
			if st.Backend != workflow.StateFile {
				return fmt.Errorf(
					"this workflow uses the %s state bus, which keeps nothing after the run — set shared_state.backend to %q to get a log",
					st.Backend, workflow.StateFile)
			}

			bus, err := workflow.OpenBus(st, baseDir)
			if err != nil {
				return err
			}
			defer bus.Close()

			seen := 0
			print := func() error {
				msgs, err := bus.Messages()
				if err != nil {
					return err
				}
				for _, m := range msgs[min(seen, len(msgs)):] {
					if jsonOut {
						enc := json.NewEncoder(os.Stdout)
						if err := enc.Encode(m); err != nil {
							return err
						}
						continue
					}
					fmt.Printf("%s  %s → %s", m.Timestamp.Local().Format("15:04:05"), m.From, m.To)
					if m.RoutingContext.TransformApplied {
						fmt.Printf("  (transformed)")
					}
					if m.Payload.Metadata.Device != "" {
						fmt.Printf("  [%s/%s, %d tok]",
							m.Payload.Metadata.Runtime, m.Payload.Metadata.Device, m.Payload.Metadata.TokensUsed)
					}
					fmt.Printf("\n%s\n\n", indent(strings.TrimSpace(m.Payload.Content)))
				}
				seen = len(msgs)
				return nil
			}

			if err := print(); err != nil {
				return err
			}
			if !follow {
				return nil
			}
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					if err := print(); err != nil {
						return err
					}
				}
			}
		},
	}
	// -f is --file on every other compose subcommand, but on a logs command
	// it universally means --follow. Follow wins here, and --file keeps
	// only its long form, rather than the two silently colliding.
	cmd.Flags().StringVar(&file, "file", workflow.FileName, "workflow file")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new messages")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit one JSON message per line")
	return cmd
}

// --- build / push / pull --------------------------------------------------

func newComposeBuildCmd() *cobra.Command {
	var file, tag string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Package a workflow as an OCI artifact",
		Long: `Build packages the workflow file itself. The units it names travel
separately, resolved from the registry at run time — they are already
content-addressed artifacts, and embedding copies of them would pin a
workflow to unit revisions its author never asked for.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			built, err := workflow.Build(cmd.Context(), s, file)
			if err != nil {
				return err
			}
			fmt.Printf("Built %s\n  digest: %s\n  agents: %d\n", built.Ref, built.Digest, built.Agents)
			if tag != "" {
				digest, perr := unit.Push(cmd.Context(), s, built.Ref, tag)
				if perr != nil {
					return perr
				}
				fmt.Printf("Pushed %s\n  digest: %s\n", tag, digest)
			}
			return nil
		},
	}
	composeFileFlag(cmd, &file)
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "also push to this registry reference")
	return cmd
}

func newComposePushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push <ref> <registry-ref>",
		Short: "Push a built workflow to an OCI registry",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			// Refuse to push a unit through the workflow command: the two
			// artifact types are pulled and run by different code paths.
			if _, err := workflow.Resolve(cmd.Context(), s, args[0]); err != nil {
				return err
			}
			logf("pushing %s → %s", args[0], args[1])
			digest, err := unit.Push(cmd.Context(), s, args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("Pushed %s\n  digest: %s\n", args[1], digest)
			return nil
		},
	}
}

func newComposePullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <registry-ref>",
		Short: "Pull a workflow from an OCI registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			logf("pulling %s", args[0])
			ref, err := unit.Pull(cmd.Context(), s, args[0])
			if err != nil {
				return err
			}
			spec, err := workflow.Resolve(cmd.Context(), s, ref)
			if err != nil {
				return err
			}
			fmt.Printf("Pulled %s (%d agents)\n  run it: %s compose up -f %s\n",
				ref, len(spec.Agents), binName, ref)
			return nil
		},
	}
}

// --- init -----------------------------------------------------------------

const scaffoldWorkflow = `apiVersion: nexusrun.dev/v1
kind: Workflow

name: %s
version: 0.1.0
description: A NexusRun workflow

agents:
  researcher:
    unit: researcher:0.1.0
    hardware:
      prefer: [gpu, cpu]

  writer:
    unit: writer:0.1.0
    # Routing below already makes this depend on researcher; depends_on is
    # for ordering that is not also data flow.
    env:
      TONE: technical

routing:
  - from: researcher
    to: writer
    # Conditions read agent.field: output, tokens, tok_per_sec, took_ms,
    # device, backend, ok. Functions: len, contains, matches, lower, upper,
    # trim, words, lines.
    condition: "len(researcher.output) > 200"
    # Transforms are Go text/template over the same values.
    transform: |
      Write an article from these notes:

      {{ summarize 2000 .researcher.output }}

shared_state:
  # memory is ephemeral; file keeps an append-only log you can read back
  # with "nexus compose logs".
  backend: file
  path: ./.nexus/state.jsonl

network:
  isolation: process
  allow_loopback: true
`

func newComposeInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a nexus-compose.yaml",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			abs, _ := filepath.Abs(dir)
			name := strings.ToLower(filepath.Base(abs))
			path := filepath.Join(dir, workflow.FileName)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists", path)
			}
			body := fmt.Sprintf(scaffoldWorkflow, name)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return err
			}
			fmt.Printf("Created %s\n\nNext:\n  %s compose validate %s\n  %s compose up -f %s -i \"your input\"\n",
				path, binName, path, binName, path)
			return nil
		},
	}
}
