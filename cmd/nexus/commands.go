package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/verdictlayer/nexusrun/internal/bench"
	"github.com/verdictlayer/nexusrun/internal/compose"
	"github.com/verdictlayer/nexusrun/internal/daemon"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/eval"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/runner"
	"github.com/verdictlayer/nexusrun/internal/sandbox"
	"github.com/verdictlayer/nexusrun/internal/server"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// --- init -----------------------------------------------------------------

const scaffoldManifest = `apiVersion: nexusrun.dev/v1
name: %s
version: 0.1.0
description: A NexusRun unit
license: MIT

models:
  - id: main
    # Reuses a model already pulled by Ollama — no second download.
    # Other sources: hf:<org>/<repo>/<file>.gguf, https://…, ./local.gguf
    source: %s
    context: 4096

entrypoint:
  type: chat
  system_prompt: |
    You are a helpful assistant.

hardware:
  # Try the NPU first, then GPU, then CPU.
  prefer: [npu, gpu, cpu]
`

// scaffoldModelSource turns a --model value into a manifest source.
//
// A bare name like "llama3.1:8b" means an Ollama model, but anything that
// already carries a scheme or looks like a path must be passed through
// untouched. The scaffold comment advertises hf:, https:// and local
// files, so unconditionally prefixing produced sources like
// "ollama:hf:org/repo/file.gguf" that could never resolve.
func scaffoldModelSource(v string) string {
	for _, scheme := range []string{"ollama:", "hf:", "http://", "https://"} {
		if strings.HasPrefix(v, scheme) {
			return v
		}
	}
	if strings.HasPrefix(v, ".") || strings.HasPrefix(v, "/") ||
		strings.Contains(v, `\`) || strings.HasSuffix(v, ".gguf") {
		return v
	}
	return "ollama:" + v
}

func newInitCmd() *cobra.Command {
	var model string
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a new unit",
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
			path := filepath.Join(dir, manifest.FileName)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists", path)
			}
			body := fmt.Sprintf(scaffoldManifest, name, scaffoldModelSource(model))
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return err
			}
			fmt.Printf("Created %s\n\nNext:\n  %s build %s\n  %s run %s:0.1.0 -p \"hello\"\n",
				path, binName, dir, binName, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "llama3.1:8b", "model source to scaffold with")
	return cmd
}

// --- build ----------------------------------------------------------------

func newBuildCmd() *cobra.Command {
	var seal bool
	var out string
	cmd := &cobra.Command{
		Use:   "build [dir]",
		Short: "Package a unit directory into an OCI artifact",
		Long: `Build packages a unit into an OCI artifact in the local store.

By default models are linked by source, keeping artifacts tiny (kilobytes).
Use --seal to embed model weights, producing a fully self-contained artifact
that runs with no network access — ideal for air-gapped or edge deployment.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			s, err := store.Open()
			if err != nil {
				return err
			}
			built, err := unit.Build(cmd.Context(), s, dir, unit.BuildOptions{
				Seal: seal, Out: out, Progress: logf,
			})
			if err != nil {
				return err
			}
			fmt.Printf("\nBuilt %s\n  digest: %s\n  layers: %d\n  sealed: %t\n",
				built.Ref, built.Digest, built.LayerCount, built.Sealed)
			return nil
		},
	}
	cmd.Flags().BoolVar(&seal, "seal", false, "embed model weights for offline/air-gapped use")
	cmd.Flags().StringVarP(&out, "output", "o", "", "also write a portable .nx file")
	return cmd
}

// --- run ------------------------------------------------------------------

func newRunCmd() *cobra.Command {
	var prompt, device, backendName string
	var maxTokens int
	var jsonOut, noDaemon, noSandbox bool
	cmd := &cobra.Command{
		Use:   "run <ref|dir>",
		Short: "Run a unit on the best available hardware",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			ref := args[0]

			// Prefer a running daemon: it already holds the weights. A unit
			// that declares tools is the exception — the daemon has no
			// tool-calling path, so asking it would silently produce an
			// answer from a model that was never offered the tools. The
			// peek is a manifest read from the local store; errors are
			// ignored here and reported properly by the path below.
			if !noDaemon && !unitDeclaresTools(ctx, s, ref) {
				dreq := daemon.RunRequest{
					Unit: ref, Prompt: prompt, MaxTokens: maxTokens, Device: device,
				}
				if dreq.Prompt == "" {
					dreq.Prompt = "Hello!"
				}
				if res := tryDaemon(ctx, dreq); res != nil {
					if jsonOut {
						enc := json.NewEncoder(os.Stdout)
						enc.SetIndent("", "  ")
						return enc.Encode(res)
					}
					state := "cold load"
					if res.Warm {
						state = "warm"
					}
					fmt.Println(res.Text)
					fmt.Printf("\n— %d tokens · %.1f tok/s · %s on %s (daemon, %s)\n",
						res.TokensOut, res.EvalTPS, res.Backend, strings.ToUpper(res.Device), state)
					return nil
				}
			}

			// A directory argument runs straight from source, skipping the
			// build step — the fast inner loop while developing a unit.
			var m *manifest.Manifest
			sealed := map[string]string{}
			srcDir := ""
			if info, statErr := os.Stat(ref); statErr == nil && info.IsDir() {
				m, err = manifest.Load(ref)
				if err != nil {
					return err
				}
				srcDir, _ = filepath.Abs(ref)
			} else {
				m, _, err = unit.Resolve(ctx, s, ref)
				if err != nil {
					return err
				}
				tmp, err := os.MkdirTemp("", "nexus-run-*")
				if err != nil {
					return err
				}
				defer os.RemoveAll(tmp)
				if sealed, err = unit.Unpack(ctx, s, ref, tmp); err != nil {
					return err
				}
				srcDir = tmp
			}

			// Script units execute a program instead of prompting a model.
			if m.Entrypoint.Type == "script" {
				workDir := srcDir
				if workDir == "" {
					return fmt.Errorf("script units must be run from a built unit or a directory")
				}
				modelPath := ""
				if len(m.Models) > 0 {
					if mp, ok := sealed[m.Models[0].ID]; ok {
						modelPath = mp
					} else if resolved, rerr := s.Resolve(m.Models[0].Source, m.Models[0].SHA256, nil); rerr == nil {
						modelPath = resolved.Path
					}
				}
				self, _ := os.Executable()
				res, rerr := runner.Run(ctx, m, runner.Options{
					WorkDir:   workDir,
					ModelPath: modelPath,
					Prompt:    prompt,
					Device:    "cpu",
					MaxTokens: maxTokens,
					NoSandbox: noSandbox,
					SelfExe:   self,
					HomeDir:   userHomeDir(),
				})
				if res != nil {
					fmt.Print(res.Output)
				}
				return rerr
			}

			if len(m.Models) == 0 {
				return fmt.Errorf("unit %s declares no models", m.Ref())
			}
			mod := m.Models[0]

			modelPath, ok := sealed[mod.ID]
			if !ok {
				resolved, err := s.Resolve(mod.Source, mod.SHA256, nil)
				if err != nil {
					return err
				}
				modelPath = resolved.Path
				if resolved.Shared {
					logf("using existing model at %s", resolved.Path)
				}
			}

			hw := hardware.Detect()
			prefer := m.Hardware.Prefer
			if device != "" {
				prefer = []string{device}
			}

			// A unit that declares tools is scheduled over the smaller set
			// of backends that can carry them. Selecting from the usual set
			// and hoping would land on llama-cli, which would generate a
			// perfectly fluent answer having never been told the tools
			// exist — the failure this whole path is arranged to prevent.
			candidates := engine.All()
			if len(m.Tools) > 0 {
				candidates = engine.ToolCapable()
				defer engine.ShutdownAll(candidates)
			}
			if backendName != "" {
				var picked []engine.Backend
				for _, cand := range candidates {
					if cand.Name() == backendName {
						picked = append(picked, cand)
					}
				}
				if len(picked) == 0 {
					// Previously an unknown name was ignored and the
					// auto-selected backend used instead, so a typo looked
					// like it had been honoured.
					var names []string
					for _, cand := range candidates {
						names = append(names, cand.Name())
					}
					return fmt.Errorf("unknown backend %q for this unit; candidates: %s",
						backendName, strings.Join(names, ", "))
				}
				candidates = picked
			}

			b, chosen, err := engine.SelectFrom(candidates, hw, prefer)
			if err != nil {
				if len(m.Tools) > 0 {
					return fmt.Errorf("%s declares %d tool(s), which needs a backend that can carry tool calls: %w",
						m.Ref(), len(m.Tools), err)
				}
				return err
			}
			logf("running %s on %s via %s", m.Ref(), strings.ToUpper(chosen), b.Name())

			tools := toolDefs(m.Tools)
			if len(tools) > 0 {
				// The candidate set should make this impossible; assert it
				// anyway, because the failure it guards against is a unit
				// answering as though it had declared no tools at all.
				if !b.Probe().SupportsTools {
					return fmt.Errorf("backend %s cannot carry tool calls, and %s declares %d",
						b.Name(), m.Ref(), len(tools))
				}
				logf("offering %d tool(s): %s", len(tools), strings.Join(toolNames(m.Tools), ", "))
			}

			p := prompt
			if p == "" {
				p = "Hello!"
			}
			temp := 0.7
			if mod.Temperature != nil {
				temp = *mod.Temperature
			}

			runID := time.Now().UTC().Format("20060102T150405Z") + "-" + sanitize(m.Name)
			rec := &store.RunRecord{
				ID: runID, Unit: m.Ref(), Started: time.Now(),
				Device: chosen, Backend: b.Name(),
			}

			res, err := b.Generate(ctx, engine.Request{
				ModelPath:   modelPath,
				ModelRef:    mod.Source,
				Prompt:      p,
				System:      m.Entrypoint.SystemPrompt,
				MaxTokens:   maxTokens,
				Temperature: temp,
				Context:     mod.Context,
				Device:      chosen,
				Chat:        m.Entrypoint.Type == "chat",
				Tools:       tools,
				Stream: func(chunk string) {
					if !jsonOut {
						fmt.Print(chunk)
					}
				},
			})
			rec.Ended = time.Now()
			if err != nil {
				rec.Error = err.Error()
				rec.ExitCode = 1
				_ = s.SaveRun(rec)
				return err
			}
			rec.TokensOut = res.TokensOut
			rec.TokPerSec = res.EvalTPS

			// The model asked to call a tool. Executing one is the next
			// piece of work; until it exists, say exactly what was asked
			// for and fail, because a unit whose tool call was dropped has
			// not done its job and must not report success.
			var toolErr error
			if len(res.ToolCalls) > 0 {
				var summaries []string
				for _, c := range res.ToolCalls {
					summaries = append(summaries, c.Summary())
				}
				toolErr = fmt.Errorf(
					"the model asked to call %s, but tool execution is not implemented yet — see docs/TOOLS.md",
					strings.Join(summaries, ", "))
				rec.Error = toolErr.Error()
				rec.ExitCode = 1
			}
			if err := s.SaveRun(rec); err != nil {
				logf("warning: could not save run record: %v", err)
			}
			_ = os.WriteFile(s.LogPath(runID), []byte(res.Text), 0o644)

			if toolErr != nil && !jsonOut {
				return toolErr
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			fmt.Printf("\n\n— %d tokens · %.1f tok/s · %s on %s\n",
				res.TokensOut, res.EvalTPS, res.Backend, strings.ToUpper(res.Device))
			return toolErr
		},
	}
	cmd.Flags().StringVarP(&prompt, "prompt", "p", "", "prompt to send")
	cmd.Flags().StringVar(&device, "device", "", "force device: npu, gpu, or cpu")
	cmd.Flags().StringVar(&backendName, "backend", "", "force backend: llama.cpp, ollama, onnxruntime")
	cmd.Flags().IntVarP(&maxTokens, "max-tokens", "n", 256, "maximum tokens to generate")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	cmd.Flags().BoolVar(&noDaemon, "no-daemon", false, "ignore a running daemon and execute directly")
	cmd.Flags().BoolVar(&noSandbox, "no-sandbox", false, "run script units without confinement (trusted units only)")
	return cmd
}

// unitDeclaresTools reports whether a unit reference or directory declares
// any tools. It answers false on any error: the caller uses it only to skip
// an optimisation, and the real load path reports the failure properly.
func unitDeclaresTools(ctx context.Context, s *store.Store, ref string) bool {
	if info, err := os.Stat(ref); err == nil && info.IsDir() {
		m, err := manifest.Load(ref)
		return err == nil && len(m.Tools) > 0
	}
	m, _, err := unit.Resolve(ctx, s, ref)
	return err == nil && len(m.Tools) > 0
}

// toolDefs converts a unit's tool declarations into what the engine offers
// the model. Only the parts the model needs to choose a tool cross over —
// how a tool is executed, and what it is allowed to touch, are the
// runtime's business and never reach the model.
func toolDefs(tools []manifest.Tool) []engine.ToolDef {
	if len(tools) == 0 {
		return nil
	}
	out := make([]engine.ToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, engine.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return out
}

func toolNames(tools []manifest.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

// newSandboxExecCmd is an internal re-exec target. It applies the unit's
// sandbox policy to itself and then becomes the unit's interpreter, so
// the restrictions are inherited and cannot be dropped.
func newSandboxExecCmd() *cobra.Command {
	var workdir, homeDir string
	var network, storage bool
	var readPaths []string
	cmd := &cobra.Command{
		Use:    runner.HelperCommandName(),
		Short:  "internal: apply sandbox policy and exec",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("no command to execute")
			}
			return runner.RunHelper(runner.HelperOptions{
				WorkDir:   workdir,
				HomeDir:   homeDir,
				Network:   network,
				Storage:   storage,
				ReadPaths: readPaths,
			}, args)
		},
	}
	cmd.Flags().StringVar(&workdir, "workdir", "", "unit working directory")
	cmd.Flags().StringVar(&homeDir, "home", "", "real user home, granted by the storage capability")
	cmd.Flags().BoolVar(&network, "network", false, "allow outbound TCP")
	cmd.Flags().BoolVar(&storage, "storage", false, "allow access to the user home directory")
	cmd.Flags().StringArrayVar(&readPaths, "read", nil, "additional read-only paths")
	return cmd
}

// --- compose --------------------------------------------------------------

func newComposeCmd() *cobra.Command {
	var input string
	var maxTokens int
	var showStages bool
	cmd := &cobra.Command{
		Use:   "compose <unit> <unit> [unit...]",
		Short: "Chain units into a pipeline, piping each output to the next",
		Long: `Compose runs units in sequence. The first receives --input; every
later unit receives the previous unit's output as its prompt.

Example:
  nexus compose summarizer:0.1.0 translator:0.1.0 --input "$(cat report.txt)"`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}

			// Each stage prefers the daemon (warm weights) and falls back
			// to direct execution, exactly like `nexus run`.
			runStage := func(ctx context.Context, ref, prompt string) (string, float64, error) {
				if res := tryDaemon(ctx, daemon.RunRequest{
					Unit: ref, Prompt: prompt, MaxTokens: maxTokens,
				}); res != nil {
					return res.Text, res.EvalTPS, nil
				}
				out, tps, rerr := runUnitDirect(ctx, s, ref, prompt, maxTokens)
				return out, tps, rerr
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
	return cmd
}

// runUnitDirect executes a chat unit without a daemon, returning its text
// and measured throughput. It is the fallback path for compose stages.
func runUnitDirect(ctx context.Context, s *store.Store, ref, prompt string, maxTokens int) (string, float64, error) {
	var m *manifest.Manifest
	var err error
	if info, statErr := os.Stat(ref); statErr == nil && info.IsDir() {
		m, err = manifest.Load(ref)
	} else {
		m, _, err = unit.Resolve(ctx, s, ref)
	}
	if err != nil {
		return "", 0, err
	}
	if len(m.Models) == 0 {
		return "", 0, fmt.Errorf("unit %s declares no models", m.Ref())
	}
	mod := m.Models[0]
	resolved, err := s.Resolve(mod.Source, mod.SHA256, nil)
	if err != nil {
		return "", 0, err
	}
	b, device, err := engine.Select(hardware.Detect(), m.Hardware.Prefer)
	if err != nil {
		return "", 0, err
	}
	temp := 0.7
	if mod.Temperature != nil {
		temp = *mod.Temperature
	}
	res, err := b.Generate(ctx, engine.Request{
		ModelPath:   resolved.Path,
		ModelRef:    mod.Source,
		Prompt:      prompt,
		System:      m.Entrypoint.SystemPrompt,
		MaxTokens:   maxTokens,
		Temperature: temp,
		Context:     mod.Context,
		Device:      device,
		Chat:        m.Entrypoint.Type == "chat",
	})
	if err != nil {
		return "", 0, err
	}
	return res.Text, res.EvalTPS, nil
}

// --- eval -----------------------------------------------------------------

// evalBackends returns the backend set for evaluation, plus a cleanup.
//
// It prefers llama-server where the plain scheduler prefers the CLI, and
// the reason is the workload: a suite is dozens of prompts against one
// model, so the CLI's per-call weight reload is paid dozens of times. A
// warm server pays it once. The CLI is used only when no server exists,
// and never alongside it — two llama.cpp targets on the same device would
// measure the same engine twice and call it two data points.
func evalBackends() ([]engine.Backend, func()) {
	ls := &engine.LlamaServer{}
	if ls.Probe().Available {
		return []engine.Backend{ls, &engine.Ollama{}, &engine.ONNXRuntime{}}, ls.Shutdown
	}
	return []engine.Backend{&engine.LlamaCPP{}, &engine.Ollama{}, &engine.ONNXRuntime{}}, func() {}
}

func newEvalCmd() *cobra.Command {
	var suitePath, device, backendName, compare string
	var repeats int
	var allDevices, jsonOut, noSave bool
	var failUnder float64

	cmd := &cobra.Command{
		Use:   "eval <ref|dir>",
		Short: "Score a unit against an eval suite on this hardware",
		Long: `Eval runs a suite of cases against a unit and reports a pass rate keyed to
the exact conditions that produced it: unit digest, model weights, backend,
device, and host.

That keying is the point. A local agent's answers change with the
quantization of its weights and the device it runs on, not just with its
prompt — a unit that passes at q8 on a workstation can fail at q3 on a Pi.
A bare percentage hides that; ` + "`--all-devices`" + ` exposes it.

Suites live in evals/ inside the unit, so they are packed into the artifact
and travel with it: whoever pulls the unit can rerun the evaluation instead
of trusting a published number.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			ref := args[0]

			// A directory is evaluated straight from source — the inner
			// loop while writing cases. A ref is unpacked so its suite
			// (packed in the source layer) can be read.
			var m *manifest.Manifest
			sealed := map[string]string{}
			srcDir := ""
			unitDigest := ""
			if info, statErr := os.Stat(ref); statErr == nil && info.IsDir() {
				if m, err = manifest.Load(ref); err != nil {
					return err
				}
				srcDir, _ = filepath.Abs(ref)
				logf("evaluating unbuilt directory %s — the score is not pinned to a unit digest", srcDir)
			} else {
				if m, _, err = unit.Resolve(ctx, s, ref); err != nil {
					return err
				}
				if unitDigest, err = unit.Digest(ctx, s, ref); err != nil {
					return err
				}
				tmp, terr := os.MkdirTemp("", "nexus-eval-*")
				if terr != nil {
					return terr
				}
				defer os.RemoveAll(tmp)
				if sealed, err = unit.Unpack(ctx, s, ref, tmp); err != nil {
					return err
				}
				srcDir = tmp
			}

			if m.Entrypoint.Type != "chat" {
				return fmt.Errorf("eval supports chat units; %s has entrypoint.type %q, which runs its own program rather than generating from a prompt",
					m.Ref(), m.Entrypoint.Type)
			}
			if len(m.Models) == 0 {
				return fmt.Errorf("unit %s declares no models", m.Ref())
			}

			if suitePath == "" {
				if suitePath, err = eval.Discover(srcDir); err != nil {
					return err
				}
			}
			suite, err := eval.Load(suitePath)
			if err != nil {
				return err
			}
			// Record the suite's path within the unit, not the temporary
			// directory a ref was unpacked into — "evals/suite.yaml" is
			// provenance, "/tmp/nexus-eval-4148871461/…" is noise.
			suiteRecorded := suitePath
			if rel, rerr := filepath.Rel(srcDir, suitePath); rerr == nil && !strings.HasPrefix(rel, "..") {
				suiteRecorded = rel
			}

			mod := m.Models[0]
			modelPath, ok := sealed[mod.ID]
			modelDigest := ""
			if !ok {
				resolved, rerr := s.Resolve(mod.Source, mod.SHA256, nil)
				if rerr != nil {
					return rerr
				}
				modelPath, modelDigest = resolved.Path, resolved.Digest
			}

			backends, cleanup := evalBackends()
			defer cleanup()
			if backendName != "" {
				var picked []engine.Backend
				for _, b := range backends {
					if b.Name() == backendName {
						picked = append(picked, b)
					}
				}
				if len(picked) == 0 {
					var names []string
					for _, b := range backends {
						names = append(names, b.Name())
					}
					return fmt.Errorf("backend %q is not available here; usable backends: %s",
						backendName, strings.Join(names, ", "))
				}
				backends = picked
			}

			prefer := m.Hardware.Prefer
			var devices []string
			if device != "" {
				prefer, devices = []string{device}, []string{device}
			}

			rep, err := eval.Run(ctx, eval.Options{
				Suite:       suite,
				SuitePath:   suiteRecorded,
				UnitRef:     m.Ref(),
				UnitDigest:  unitDigest,
				ModelPath:   modelPath,
				ModelRef:    mod.Source,
				ModelDigest: modelDigest,
				System:      m.Entrypoint.SystemPrompt,
				Context:     mod.Context,
				Chat:        true,
				Repeats:     repeats,
				Devices:     devices,
				AllDevices:  allDevices,
				Prefer:      prefer,
				Backends:    backends,
				Progress:    logf,
			})
			if err != nil {
				return err
			}

			// Compare before printing the scorecard so the delta is the
			// last thing on screen — it is what the reader came for.
			var diff *eval.DiffReport
			if compare != "" {
				var before *eval.Report
				if compare == "latest" {
					if before, err = eval.Latest(s, rep.Unit); err != nil {
						return err
					}
					if before == nil {
						logf("no earlier saved evaluation for %s to compare against", rep.Unit)
					}
				} else if before, err = eval.LoadRecord(s, compare); err != nil {
					return err
				}
				if before != nil {
					diff = eval.Diff(before, rep)
				}
			}

			if !noSave {
				if err := eval.Save(s, rep); err != nil {
					logf("warning: could not save evaluation: %v", err)
				} else {
					logf("saved as %s", rep.ID)
				}
			}

			if jsonOut {
				out := map[string]any{"report": rep}
				if diff != nil {
					out["diff"] = diff
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(out); err != nil {
					return err
				}
			} else {
				fmt.Printf("\n%s", rep.String())
				if diff != nil {
					fmt.Printf("\n%s", diff.String())
				}
			}

			// Exit status makes the suite usable as a gate in CI.
			best := rep.Best()
			if best == nil {
				return fmt.Errorf("no target could execute the suite")
			}
			if failUnder > 0 && best.Rate() < failUnder {
				return fmt.Errorf("pass rate %.1f%% is below --fail-under %.1f%%", best.Rate(), failUnder)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&suitePath, "suite", "", "suite file (default: the unit's evals/ directory)")
	cmd.Flags().StringVar(&device, "device", "", "restrict to one device: npu, gpu, or cpu")
	cmd.Flags().StringVar(&backendName, "backend", "", "restrict to one backend")
	cmd.Flags().IntVar(&repeats, "repeats", 1, "runs per case; above 1, cases that don't pass every run are reported flaky")
	cmd.Flags().BoolVar(&allDevices, "all-devices", false, "evaluate on every usable backend/device pair, not just the one the unit would pick")
	cmd.Flags().Float64Var(&failUnder, "fail-under", 0, "exit non-zero if the best pass rate is below this percentage")
	cmd.Flags().StringVar(&compare, "compare", "", "diff against a saved evaluation ID (bare flag: the latest for this unit)")
	cmd.Flags().Lookup("compare").NoOptDefVal = "latest"
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&noSave, "no-save", false, "do not save the report")
	cmd.AddCommand(newEvalListCmd(), newEvalDiffCmd())
	return cmd
}

func newEvalListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List saved evaluations, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			reports, err := eval.List(s)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(reports)
			}
			if len(reports) == 0 {
				fmt.Printf("No evaluations yet. Run `%s eval <unit>` to create one.\n", binName)
				return nil
			}
			// Widths follow the data: IDs carry a suite slug of any
			// length, and a wrapped table is unreadable.
			idW, unitW, suiteW := len("ID"), len("UNIT"), len("SUITE")
			for _, r := range reports {
				idW = max(idW, len(r.ID))
				unitW = max(unitW, len(r.Unit))
				suiteW = max(suiteW, len(r.Suite))
			}
			row := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%7s  %%s\n", idW, unitW, suiteW)
			fmt.Printf(row, "ID", "UNIT", "SUITE", "BEST", "TARGET")
			for _, r := range reports {
				rate, target := "—", "—"
				if best := r.Best(); best != nil {
					rate = fmt.Sprintf("%.1f%%", best.Rate())
					target = best.Label()
				}
				fmt.Printf(row, r.ID, r.Unit, r.Suite, rate, target)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func newEvalDiffCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "diff <before-id> <after-id>",
		Short: "Compare two saved evaluations case by case",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			before, err := eval.LoadRecord(s, args[0])
			if err != nil {
				return err
			}
			after, err := eval.LoadRecord(s, args[1])
			if err != nil {
				return err
			}
			d := eval.Diff(before, after)
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(d)
			}
			fmt.Print(d.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

// --- list / inspect -------------------------------------------------------

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List locally stored units",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			refs, err := unit.List(cmd.Context(), s)
			if err != nil {
				return err
			}
			if len(refs) == 0 {
				fmt.Printf("No units yet. Create one:\n  %s init my-agent && %s build my-agent\n", binName, binName)
				return nil
			}
			for _, r := range refs {
				m, om, err := unit.Resolve(cmd.Context(), s, r)
				if err != nil {
					fmt.Printf("%-28s (unreadable)\n", r)
					continue
				}
				var size int64
				for _, l := range om.Layers {
					size += l.Size
				}
				sealed := om.Annotations[unit.AnnotationSealed] == "true"
				kind := "linked"
				if sealed {
					kind = "sealed"
				}
				fmt.Printf("%-28s %-8s %-10s %s\n", r, kind, humanSize(size), m.Description)
			}
			return nil
		},
	}
}

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <ref>",
		Short: "Show a unit's manifest and OCI layers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			m, om, err := unit.Resolve(cmd.Context(), s, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Unit:        %s\n", m.Ref())
			if m.Description != "" {
				fmt.Printf("Description: %s\n", m.Description)
			}
			fmt.Printf("Artifact:    %s\n", om.ArtifactType)
			fmt.Printf("Prefer:      %s\n\nModels:\n", strings.Join(m.Hardware.Prefer, " → "))
			for _, mod := range m.Models {
				fmt.Printf("  %-10s %s (%s)\n", mod.ID, mod.Source, mod.Format)
			}
			fmt.Printf("\nLayers:\n")
			for _, l := range om.Layers {
				fmt.Printf("  %-52s %-10s %s\n", l.MediaType, humanSize(l.Size), shortDigest(l.Digest.String()))
			}
			return nil
		},
	}
}

// --- registry -------------------------------------------------------------

func newPushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push <ref> <registry-ref>",
		Short: "Push a unit to any OCI registry",
		Long: `Push a unit to an OCI registry such as ghcr.io, Docker Hub, ECR, or Harbor.

Credentials come from NEXUSRUN_REGISTRY_USER / NEXUSRUN_REGISTRY_PASSWORD
(GITHUB_TOKEN also works for ghcr.io).

Example:
  nexus push my-agent:0.1.0 ghcr.io/you/my-agent:0.1.0`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
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

func newPullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <registry-ref>",
		Short: "Pull a unit from an OCI registry",
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
			fmt.Printf("Pulled %s\n  run it: %s run %s -p \"hello\"\n", ref, binName, ref)
			return nil
		},
	}
}

func newExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <ref> <file.nx>",
		Short: "Export a unit to a portable file (sneakernet / air-gap)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			if err := unit.ExportFile(cmd.Context(), s, args[0], args[1]); err != nil {
				return err
			}
			info, _ := os.Stat(args[1])
			fmt.Printf("Exported %s → %s (%s)\n", args[0], args[1], humanSize(info.Size()))
			return nil
		},
	}
}

func newImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <file.nx>",
		Short: "Import a unit from a portable file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			refs, err := unit.ImportFile(cmd.Context(), s, args[0])
			if err != nil {
				return err
			}
			for _, r := range refs {
				fmt.Printf("Imported %s\n", r)
			}
			return nil
		},
	}
}

// --- hardware -------------------------------------------------------------

func newDoctorCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report detected hardware and which backends can actually use it",
		Long: `Doctor separates two things people conflate:

  detected hardware — what the machine physically has
  backend capability — what the installed runtimes can actually execute on

A GPU that llama.cpp was not compiled for, or an NPU with no execution
provider installed, shows up in the first list but not the second.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			hw := hardware.Detect()
			if jsonOut {
				out := map[string]any{"hardware": hw}
				out["backends"] = engine.ProbeAll()
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			fmt.Printf("Detected hardware\n%s\n", indent(hw.String()))
			fmt.Printf("Backend capability\n")
			usable := map[string]bool{}
			caps := engine.ProbeAll()
			// Width follows the longest name; "llama.cpp/server" overflowed
			// the fixed column and pushed every field on its row right.
			nameW := 0
			for _, c := range caps {
				nameW = max(nameW, len(c.Backend))
			}
			for _, c := range caps {
				status := "unavailable"
				if c.Available {
					status = "ready"
				}
				fmt.Printf("  %-*s %-12s %s\n", nameW, c.Backend, status, c.Detail)
				if c.Version != "" {
					fmt.Printf("  %-*s %-12s version %s\n", nameW, "", "", c.Version)
				}
				if c.Available {
					for _, d := range c.Devices {
						if hw.Has(d) {
							usable[d] = true
						}
					}
				}
			}

			// Tool calling is capability probing applied to tools: a unit
			// that declares them needs a backend that can carry them, and
			// which of the installed ones can is not obvious.
			fmt.Printf("\nTool calling\n")
			var toolCapable []string
			for _, c := range caps {
				switch {
				case c.Available && c.SupportsTools:
					toolCapable = append(toolCapable, c.Backend)
					fmt.Printf("  %-*s %-12s native tool calls\n", nameW, c.Backend, "ready")
				case c.Available:
					fmt.Printf("  %-*s %-12s no tool-call support in this backend\n", nameW, c.Backend, "unsupported")
				}
			}
			if len(toolCapable) == 0 {
				fmt.Printf("  none — units that declare tools cannot run here.\n")
				fmt.Printf("  Install llama-server (it ships beside llama-cli) or start Ollama.\n")
			}

			fmt.Printf("\nSandboxing:     %s\n", sandbox.Describe())

			fmt.Printf("Usable devices: ")
			if len(usable) == 0 {
				fmt.Printf("none — install llama.cpp to get started\n")
				return nil
			}
			var list []string
			for _, class := range []string{hardware.ClassNPU, hardware.ClassGPU, hardware.ClassCPU} {
				if usable[class] {
					list = append(list, strings.ToUpper(class))
				}
			}
			fmt.Printf("%s\n", strings.Join(list, ", "))

			// Call out hardware present but unusable — the most common
			// and most confusing failure mode.
			for _, d := range hw.Devices {
				if d.Class != hardware.ClassCPU && !usable[d.Class] {
					fmt.Printf("\nNote: %s %s was detected but no installed backend can drive it.\n", d.Vendor, d.Name)
					if d.Class == hardware.ClassNPU {
						fmt.Printf("      NPU execution needs onnxruntime with the %s provider — see docs/NPU.md\n", d.Backend)
					} else {
						fmt.Printf("      Rebuild llama.cpp with %s support, or check the driver.\n", d.Backend)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func newBenchCmd() *cobra.Command {
	var model, prompt string
	var runs, maxTokens int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Measure real throughput on every usable device",
		Long: `Bench runs the same prompt on every backend/device pair this host can
actually execute on, and reports median tokens/sec.

This is how you answer "is the NPU actually faster than the CPU here?"
empirically, instead of trusting spec sheets.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if model == "" {
				return fmt.Errorf("--model is required (a .gguf path or ollama:<name>)")
			}
			s, err := store.Open()
			if err != nil {
				return err
			}
			resolved, err := s.Resolve(model, "", nil)
			if err != nil {
				return err
			}
			rep, err := bench.Run(cmd.Context(), bench.Options{
				ModelPath: resolved.Path,
				Prompt:    prompt,
				Runs:      runs,
				MaxTokens: maxTokens,
				Progress:  logf,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			fmt.Printf("\n%s", rep.String())
			return nil
		},
	}
	cmd.Flags().StringVarP(&model, "model", "m", "", "model to benchmark (path or ollama:<name>)")
	cmd.Flags().StringVarP(&prompt, "prompt", "p", "", "prompt to use")
	cmd.Flags().IntVar(&runs, "runs", 3, "repetitions per device (median reported)")
	cmd.Flags().IntVarP(&maxTokens, "max-tokens", "n", 64, "tokens to generate per run")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func newModelsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List models available locally (including Ollama's)",
		RunE: func(cmd *cobra.Command, args []string) error {
			models := store.ListOllamaModels()
			if len(models) == 0 {
				fmt.Println("No local models found.")
				fmt.Println("NexusRun can reuse models from Ollama, or download from Hugging Face:")
				fmt.Println("  source: hf:bartowski/Llama-3.2-1B-Instruct-GGUF/Llama-3.2-1B-Instruct-Q4_K_M.gguf")
				return nil
			}
			fmt.Printf("Reusable from Ollama (no re-download):\n")
			for _, m := range models {
				fmt.Printf("  ollama:%s\n", m)
			}
			return nil
		},
	}
}

// --- logs / serve ---------------------------------------------------------

func newLogsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "logs [run-id]",
		Short: "Show run history, or one run's output",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				data, err := os.ReadFile(s.LogPath(args[0]))
				if err != nil {
					return err
				}
				fmt.Print(string(data))
				return nil
			}
			runs, err := s.Runs()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(runs)
			}
			if len(runs) == 0 {
				fmt.Println("No runs yet.")
				return nil
			}
			fmt.Printf("%-34s %-22s %-6s %-12s %8s\n", "RUN", "UNIT", "DEV", "BACKEND", "TOK/S")
			for _, r := range runs {
				status := ""
				if r.Error != "" {
					status = "  (failed)"
				}
				fmt.Printf("%-34s %-22s %-6s %-12s %8.1f%s\n",
					r.ID, r.Unit, strings.ToUpper(r.Device), r.Backend, r.TokPerSec, status)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func newServeCmd() *cobra.Command {
	var addr string
	var idle time.Duration
	var noWarm bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the console and warm-model daemon",
		Long: `Serve runs the local web console and, unless --no-warm is given, an
inference daemon that keeps models resident in memory.

While it is running, ` + "`nexus run`" + ` routes through it automatically and skips
the model load on every repeat run. Stop it and everything still works —
the CLI just falls back to loading per run.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			var pool *daemon.Pool
			if !noWarm {
				pool = daemon.NewPool(s)
				if pool.Available() {
					pool.StartEviction(ctx, idle)
					defer pool.Shutdown()
					fmt.Printf("Warm-model daemon active (idle timeout %s)\n", idle)
				} else {
					fmt.Println("Warm models unavailable: llama-server not found — console only.")
					pool = nil
				}
			}
			fmt.Printf("NexusRun console → http://%s\n", addr)
			return server.Serve(ctx, s, addr, pool)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7717", "listen address")
	cmd.Flags().DurationVar(&idle, "idle-timeout", daemon.DefaultIdleTimeout, "release a model after this long unused")
	cmd.Flags().BoolVar(&noWarm, "no-warm", false, "console only; do not keep models in memory")
	return cmd
}

// daemonAddr is where the CLI looks for a running daemon.
func daemonAddr() string {
	if v := os.Getenv("NEXUSRUN_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:7717"
}

// tryDaemon sends a run to a local daemon. It returns nil (no error) when
// no daemon is listening, so the caller can fall back to direct execution.
func tryDaemon(ctx context.Context, req daemon.RunRequest) *daemon.RunResponse {
	body, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	url := "http://" + daemonAddr() + "/api/run"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// A short dial timeout keeps the "no daemon" path fast, while the
	// overall request may take as long as generation needs.
	client := &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 300 * time.Millisecond}).DialContext,
		},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out daemon.RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return &out
}

// --- shared helpers -------------------------------------------------------

// userHomeDir resolves the invoking user's home before any sandboxing
// rewrites HOME.
func userHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func indent(s string) string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, "  "+line)
	}
	return strings.Join(out, "\n") + "\n"
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
