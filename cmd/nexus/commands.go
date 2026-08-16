package main

import (
	"bytes"
	"context"
	"encoding/csv"
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

	"github.com/verdictlayer/nexusrun/internal/agent"
	"github.com/verdictlayer/nexusrun/internal/automodel"
	"github.com/verdictlayer/nexusrun/internal/bench"
	"github.com/verdictlayer/nexusrun/internal/checkpoint"
	"github.com/verdictlayer/nexusrun/internal/daemon"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/eval"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/mcp"
	"github.com/verdictlayer/nexusrun/internal/runner"
	"github.com/verdictlayer/nexusrun/internal/sandbox"
	"github.com/verdictlayer/nexusrun/internal/secrets"
	"github.com/verdictlayer/nexusrun/internal/server"
	"github.com/verdictlayer/nexusrun/internal/session"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
	"github.com/verdictlayer/nexusrun/internal/workflow"
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
	var prompt, device, backendName, sessionName, restorePath string
	var maxTokens, maxTurns, trimAt int
	var jsonOut, noDaemon, noSandbox, autoModel, refreshBench, mcpDebug bool
	var cacheTTL time.Duration
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

			// Prefer a running daemon: it already holds the weights. Two
			// kinds of unit are the exception — one declaring tools (the
			// daemon has no tool-calling path, so asking it would silently
			// answer from a model never offered the tools) and one whose
			// model is chosen by measurement (the daemon has no selection
			// path, and no single model to be warm with). The peek is a
			// manifest read from the local store; errors are ignored here
			// and reported properly by the path below.
			// A session or a restore also has to bypass the daemon: it has
			// no conversation state, so it would answer the prompt alone
			// and drop the history the user asked to continue.
			if !noDaemon && sessionName == "" && restorePath == "" && !unitNeedsDirectRun(ctx, s, ref) {
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
				// Declared secrets are resolved from the local encrypted
				// store and injected as environment. A required secret that
				// is not stored fails here, before the script starts —
				// an agent that dies mid-request against a third party is
				// far harder to diagnose.
				inj, ierr := injectFor(s, m, deviceID)
				if ierr != nil {
					return ierr
				}
				defer inj.Close()
				if len(inj.Missing) > 0 {
					logf("optional secret(s) not stored, continuing without: %s",
						strings.Join(inj.Missing, ", "))
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
					Env:       inj.Env,
					ReadPaths: inj.Files,
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

			// The hardware preference is settled before the model, because
			// auto-selection has to measure candidates on the device the
			// unit will actually run on.
			hw := hardware.Detect()
			prefer := m.Hardware.Prefer
			if device != "" {
				prefer = []string{device}
			}

			// modelSource and modelContext may be decided by measurement
			// rather than read from the manifest, so everything downstream
			// reads them instead of mod.Source / mod.Context.
			modelSource, modelContext := mod.Source, mod.Context

			var modelPath string
			switch {
			case mod.Auto():
				dec, derr := selectAutoModel(ctx, s, m, mod, srcDir, ref, prefer, autoModelOpts{
					refresh: refreshBench, ttl: cacheTTL,
				})
				if derr != nil {
					return derr
				}
				if !jsonOut {
					fmt.Fprint(os.Stderr, "\n"+dec.String()+"\n")
				}
				modelPath, modelSource = dec.Path, dec.Source
				if dec.Context > 0 {
					modelContext = dec.Context
				}

			case autoModel:
				// The flag was given but the unit names its model outright.
				// Silently ignoring it would leave the user believing a
				// selection happened.
				return fmt.Errorf(
					"--auto-model needs a model entry with candidates, but %s names %s directly — see docs/AUTOMODEL.md",
					m.Ref(), mod.Source)

			default:
				var ok bool
				if modelPath, ok = sealed[mod.ID]; !ok {
					resolved, rerr := s.Resolve(mod.Source, mod.SHA256, nil)
					if rerr != nil {
						return rerr
					}
					modelPath = resolved.Path
					if resolved.Shared {
						logf("using existing model at %s", resolved.Path)
					}
				}
			}

			// A unit that declares tools is scheduled over the smaller set
			// of backends that can carry them. Selecting from the usual set
			// and hoping would land on llama-cli, which would generate a
			// perfectly fluent answer having never been told the tools
			// exist — the failure this whole path is arranged to prevent.
			// Tools reach the model from two places — the unit's own and
			// whatever its MCP servers offer — and either one needs a
			// backend that can carry tool calls. A session needs the same
			// smaller set for a different reason: it is a conversation, and
			// the one-shot CLI can express a single turn.
			candidates := engine.All()
			switch {
			case len(m.Tools) > 0 || len(m.MCPServers) > 0:
				candidates = engine.ToolCapable()
				defer engine.ShutdownAll(candidates)
			case sessionName != "" || restorePath != "":
				candidates = engine.Conversational()
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
				if len(m.Tools) > 0 || len(m.MCPServers) > 0 {
					return fmt.Errorf("%s declares %d tool(s) and %d MCP server(s), which needs a backend that can carry tool calls: %w",
						m.Ref(), len(m.Tools), len(m.MCPServers), err)
				}
				if sessionName != "" || restorePath != "" {
					return fmt.Errorf("a session is a multi-turn conversation, which needs a backend that can carry one: %w", err)
				}
				return err
			}
			logf("running %s on %s via %s", m.Ref(), strings.ToUpper(chosen), b.Name())

			// Secrets are resolved before anything is started: an MCP
			// server usually needs the API token, and a missing required
			// one should fail here rather than three turns in.
			inj, err := injectFor(s, m, deviceID)
			if err != nil {
				return err
			}
			defer inj.Close()

			self, _ := os.Executable()

			// MCP servers come up before the model, so their tools can be
			// offered in the same list as the unit's own.
			mcpMgr, err := mcp.Start(ctx, m, mcp.StartOptions{
				Store: s, WorkDir: srcDir, Env: inj.Env,
				SelfExe: self, NoSandbox: noSandbox, HomeDir: userHomeDir(),
				AutoInstall: true, Debug: mcpDebug, Logf: logf,
			})
			if err != nil {
				return err
			}
			defer mcpMgr.Close()

			scriptTools := agent.NewScriptTools(m, agent.ScriptOptions{
				WorkDir: srcDir, HomeDir: userHomeDir(), SelfExe: self,
				NoSandbox: noSandbox, Env: inj.Env,
			})

			tools := append(toolDefs(m.Tools), agent.Defs(mcpMgr)...)
			if len(tools) > 0 {
				// The candidate set should make this impossible; assert it
				// anyway, because the failure it guards against is a unit
				// answering as though it had declared no tools at all.
				if !b.Probe().SupportsTools {
					return fmt.Errorf("backend %s cannot carry tool calls, and %s offers %d",
						b.Name(), m.Ref(), len(tools))
				}
				var names []string
				for _, t := range tools {
					names = append(names, t.Name)
				}
				logf("offering %d tool(s): %s", len(tools), strings.Join(names, ", "))
			}

			p := prompt
			if p == "" {
				p = "Hello!"
			}
			temp := 0.7
			if mod.Temperature != nil {
				temp = *mod.Temperature
			}

			// A session makes the run part of a continuing conversation
			// rather than a one-shot. Without --session or --restore this
			// is an in-memory session that is never written, so the default
			// behaviour is unchanged.
			sess, persist, err := openSession(s, m, sessionName, restorePath, modelSource, srcDir)
			if err != nil {
				return err
			}
			sess.Backend, sess.Device = b.Name(), chosen
			sess.Context = modelContext
			if trimAt > 0 {
				if dropped := sess.Trim(trimAt); dropped > 0 {
					logf("trimmed %d message(s) from the start of the session to fit --trim %d", dropped, trimAt)
				}
			}
			if len(sess.Messages) > 0 {
				logf("resuming %s with %d message(s) of history", sess.Name, len(sess.Messages))
			}
			sess.AddUser(p)

			runID := time.Now().UTC().Format("20060102T150405Z") + "-" + sanitize(m.Name)
			rec := &store.RunRecord{
				ID: runID, Unit: m.Ref(), Started: time.Now(),
				Device: chosen, Backend: b.Name(),
			}

			res, runErr := agent.Run(ctx, agent.Options{
				Backend: b,
				Request: engine.Request{
					ModelPath:   modelPath,
					ModelRef:    modelSource,
					Messages:    sess.Conversation(m.Entrypoint.SystemPrompt),
					MaxTokens:   maxTokens,
					Temperature: temp,
					Context:     modelContext,
					Device:      chosen,
					Chat:        true,
				},
				Tools:     tools,
				Executors: []agent.Executor{scriptTools, agent.NewMCPTools(mcpMgr)},
				MaxTurns:  maxTurns,
				Stream: func(chunk string) {
					if !jsonOut {
						fmt.Print(chunk)
					}
				},
				Progress: logf,
			})
			rec.Ended = time.Now()

			// Whatever happened, the turns that did complete belong in the
			// session: a run that failed on turn three is exactly the one
			// whose transcript is worth keeping.
			if res != nil {
				sess.Messages = append(sess.Messages, res.Messages...)
				sess.Turns += res.Turns
				sess.TokensOut += res.TokensOut
				if persist {
					if serr := sess.Save(s); serr != nil {
						logf("warning: could not save session %s: %v", sess.Name, serr)
					}
				}
				rec.TokensOut = res.TokensOut
				rec.TokPerSec = res.EvalTPS
			}
			if runErr != nil {
				rec.Error = runErr.Error()
				rec.ExitCode = 1
			}
			if err := s.SaveRun(rec); err != nil {
				logf("warning: could not save run record: %v", err)
			}
			if res != nil {
				_ = os.WriteFile(s.LogPath(runID), []byte(res.Text), 0o644)
			}
			if runErr != nil {
				return runErr
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			fmt.Printf("\n\n— %d tokens · %.1f tok/s · %s on %s",
				res.TokensOut, res.EvalTPS, res.Backend, strings.ToUpper(res.Device))
			if len(res.Steps) > 0 {
				fmt.Printf(" · %d tool call(s) over %d turn(s)", len(res.Steps), res.Turns)
			}
			if persist {
				fmt.Printf(" · session %s", sess.Name)
			}
			fmt.Println()
			return nil
		},
	}
	cmd.Flags().StringVarP(&prompt, "prompt", "p", "", "prompt to send")
	cmd.Flags().StringVar(&device, "device", "", "force device: npu, gpu, or cpu")
	cmd.Flags().StringVar(&backendName, "backend", "", "force backend: llama.cpp, ollama, onnxruntime")
	cmd.Flags().IntVarP(&maxTokens, "max-tokens", "n", 256, "maximum tokens to generate")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	cmd.Flags().BoolVar(&noDaemon, "no-daemon", false, "ignore a running daemon and execute directly")
	cmd.Flags().BoolVar(&noSandbox, "no-sandbox", false, "run script units without confinement (trusted units only)")
	cmd.Flags().BoolVar(&autoModel, "auto-model", false,
		"require the unit to select its model by measurement (units declaring candidates do this anyway)")
	cmd.Flags().BoolVar(&refreshBench, "refresh-bench", false,
		"re-measure candidates instead of trusting the benchmark cache")
	cmd.Flags().DurationVar(&cacheTTL, "cache-ttl", bench.DefaultTTL, "how long a cached benchmark stays trusted")
	cmd.Flags().StringVarP(&sessionName, "session", "s", "",
		"continue a named conversation, remembering it across runs")
	cmd.Flags().StringVar(&restorePath, "restore", "", "resume from a checkpoint file")
	cmd.Flags().IntVar(&maxTurns, "max-turns", agent.DefaultMaxTurns,
		"stop after this many tool-calling turns")
	cmd.Flags().IntVar(&trimAt, "trim", 0,
		"before generating, keep only the most recent N messages of the session")
	cmd.Flags().BoolVar(&mcpDebug, "mcp-debug", false, "log every MCP frame in both directions")
	return cmd
}

// openSession resolves --session and --restore into the conversation this
// run continues.
//
// With neither flag the session is in memory and never written, so a plain
// `nexus run` behaves exactly as it always did. persist reports whether the
// session is one the user asked to keep.
func openSession(s *store.Store, m *manifest.Manifest, name, restorePath, model, srcDir string) (*session.Session, bool, error) {
	if restorePath != "" {
		f, err := os.Open(restorePath)
		if err != nil {
			return nil, false, err
		}
		defer f.Close()

		modelDir := filepath.Join(s.Root, "restored")
		res, err := checkpoint.Load(f, checkpoint.LoadOptions{ModelDir: modelDir, Progress: logf})
		if err != nil {
			return nil, false, err
		}
		sess := res.Session
		logf("restored %s: %d message(s), %d turn(s), saved %s",
			sess.Name, len(sess.Messages), sess.Turns, res.Manifest.CreatedAt.Local().Format(time.RFC3339))

		// A checkpoint from a different unit still restores — carrying a
		// transcript to another agent is a legitimate thing to do — but it
		// is never silent, because the system prompt it was produced under
		// no longer applies.
		if sess.Unit != "" && sess.Unit != m.Ref() {
			logf("warning: this checkpoint was made by %s, and is being resumed on %s", sess.Unit, m.Ref())
		}
		if name != "" {
			if err := session.ValidName(name); err != nil {
				return nil, false, err
			}
			sess.Name = name
			return sess, true, nil
		}
		if sess.Name == "" {
			sess.Name = "restored"
		}
		return sess, false, nil
	}

	if name == "" {
		// Anonymous: a real session object so the code path is identical,
		// but nothing is written.
		return session.New("(anonymous)", m.Ref()), false, nil
	}
	if err := session.ValidName(name); err != nil {
		return nil, false, err
	}
	sess, err := session.Load(s, name)
	if err != nil {
		return nil, false, err
	}
	if sess == nil {
		sess = session.New(name, m.Ref())
	} else if sess.Unit != m.Ref() {
		logf("warning: session %s was started by %s, continuing it on %s", name, sess.Unit, m.Ref())
		sess.Unit = m.Ref()
	}
	sess.Model = model
	sess.System = m.Entrypoint.SystemPrompt
	return sess, true, nil
}

// deviceID names this machine for device-scoped secrets. A fleet sets it
// per device; unset means only globally scoped secrets apply.
var deviceID = os.Getenv("NEXUS_DEVICE_ID")

// injectFor resolves a unit's declared secrets and config.
//
// A unit that declares none never opens the secret store, so no master key
// is generated for someone who is not using the feature.
func injectFor(s *store.Store, m *manifest.Manifest, device string) (*secrets.Injection, error) {
	if len(m.Secrets) == 0 && len(m.Config) == 0 {
		return &secrets.Injection{}, nil
	}
	st, err := secrets.Open(s)
	if err != nil {
		return nil, err
	}
	return st.Inject(m, secrets.InjectOptions{Device: device})
}

// autoModelOpts carries the selection knobs shared by `run` and `bench`.
type autoModelOpts struct {
	refresh bool
	dryRun  bool
	ttl     time.Duration
}

// selectAutoModel resolves a candidate list to one model on this machine.
//
// The unit's own eval suite is what "quality" means, so it is discovered
// here and handed to the selector. A unit with no suite still selects —
// on throughput alone — which is why a missing suite is not an error.
func selectAutoModel(ctx context.Context, s *store.Store, m *manifest.Manifest, mod manifest.Model,
	srcDir, ref string, prefer []string, opts autoModelOpts) (*automodel.Decision, error) {

	// The unit's own suite is what "quality" means to the selector. A unit
	// with no suite still selects, on throughput alone — but a suite that
	// exists and does not parse is a hard error. Degrading to a throughput
	// ranking there would answer a different question than the one the
	// unit's requirements asked, and report it as though it were the same.
	var suite *eval.Suite
	suitePath := ""
	if srcDir != "" {
		if p, derr := eval.Discover(srcDir); derr == nil {
			loaded, lerr := eval.Load(p)
			if lerr != nil {
				return nil, fmt.Errorf("the unit's eval suite could not be loaded, so its model cannot be selected on quality: %w", lerr)
			}
			suite, suitePath = loaded, p
			if rel, rerr := filepath.Rel(srcDir, p); rerr == nil && !strings.HasPrefix(rel, "..") {
				suitePath = rel
			}
		}
	}

	// The unit digest pins a cached measurement to the exact artifact it
	// was taken against. An unbuilt directory has none, and its results are
	// cached without one rather than not at all.
	unitDigest := ""
	if info, err := os.Stat(ref); err != nil || !info.IsDir() {
		unitDigest, _ = unit.Digest(ctx, s, ref)
	}

	backends, cleanup := evalBackends()
	defer cleanup()

	return automodel.Select(ctx, automodel.Options{
		Store:      s,
		Manifest:   m,
		Model:      mod,
		Suite:      suite,
		SuitePath:  suitePath,
		UnitDigest: unitDigest,
		Backends:   backends,
		Prefer:     prefer,
		CacheTTL:   opts.ttl,
		Refresh:    opts.refresh,
		DryRun:     opts.dryRun,
		Progress:   logf,
	})
}

// unitNeedsDirectRun reports whether a unit must bypass the warm daemon:
// it declares tools, or it selects its model by measurement. It answers
// false on any error — the caller uses it only to skip an optimisation,
// and the real load path reports the failure properly.
func unitNeedsDirectRun(ctx context.Context, s *store.Store, ref string) bool {
	var m *manifest.Manifest
	var err error
	if info, serr := os.Stat(ref); serr == nil && info.IsDir() {
		m, err = manifest.Load(ref)
	} else {
		m, _, err = unit.Resolve(ctx, s, ref)
	}
	if err != nil {
		return false
	}
	// Tools and MCP servers both need the agent loop, which the daemon has
	// no path for; an auto-selecting model has no single model to be warm
	// with. Each would otherwise be answered as though it were a plain
	// one-shot prompt.
	if len(m.Tools) > 0 || len(m.MCPServers) > 0 {
		return true
	}
	for _, mod := range m.Models {
		if mod.Auto() {
			return true
		}
	}
	return false
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

// --- shared unit execution ------------------------------------------------

// runUnitDirect executes a chat unit without a daemon, returning its text
// and measured throughput. It is the fallback path for pipeline stages.
func runUnitDirect(ctx context.Context, s *store.Store, ref, prompt string, maxTokens int) (string, float64, error) {
	res, err := runAgentDirect(ctx, s, workflow.AgentRequest{
		Unit: ref, Prompt: prompt, MaxTokens: maxTokens,
	})
	if err != nil {
		return "", 0, err
	}
	return res.Output, res.TokPerSec, nil
}

// runAgentDirect executes one chat unit in-process, honouring a workflow
// agent's overrides: a different model, a different hardware preference,
// and environment the unit's own scripts may read.
func runAgentDirect(ctx context.Context, s *store.Store, req workflow.AgentRequest) (*workflow.AgentResult, error) {
	var m *manifest.Manifest
	var err error
	if info, statErr := os.Stat(req.Unit); statErr == nil && info.IsDir() {
		m, err = manifest.Load(req.Unit)
	} else {
		m, _, err = unit.Resolve(ctx, s, req.Unit)
	}
	if err != nil {
		return nil, err
	}
	if len(m.Models) == 0 {
		return nil, fmt.Errorf("unit %s declares no models", m.Ref())
	}
	mod := m.Models[0]

	// A workflow may substitute the model. That is the point of the field:
	// the same unit serves as the cheap and the expensive stage in one file.
	source, sha := mod.Source, mod.SHA256
	if req.Model != "" {
		source, sha = req.Model, ""
	} else if mod.Auto() {
		return nil, fmt.Errorf(
			"unit %s selects its model from candidates; give the agent an explicit `model:` in the workflow, or run it with `%s run %s --auto-model`",
			m.Ref(), binName, req.Unit)
	}

	resolved, err := s.Resolve(source, sha, nil)
	if err != nil {
		return nil, err
	}

	prefer := m.Hardware.Prefer
	if len(req.Prefer) > 0 {
		prefer = req.Prefer
	}
	b, device, err := engine.Select(hardware.Detect(), prefer)
	if err != nil {
		return nil, err
	}

	temp := 0.7
	if mod.Temperature != nil {
		temp = *mod.Temperature
	}
	res, err := b.Generate(ctx, engine.Request{
		ModelPath:   resolved.Path,
		ModelRef:    source,
		Prompt:      req.Prompt,
		System:      m.Entrypoint.SystemPrompt,
		MaxTokens:   req.MaxTokens,
		Temperature: temp,
		Context:     mod.Context,
		Device:      device,
		Chat:        m.Entrypoint.Type == "chat",
	})
	if err != nil {
		return nil, err
	}
	return &workflow.AgentResult{
		Output: res.Text, TokensOut: res.TokensOut, TokPerSec: res.EvalTPS,
		Backend: res.Backend, Device: res.Device,
	}, nil
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
	var extraModels []string
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

			// The models to score the suite against. With no --model the
			// unit's own is the only one; each extra is a what-if, recorded
			// as an override so its score is never mistaken for the unit's.
			type candidateModel struct {
				source   string
				digest   string
				path     string
				quant    string
				params   string
				size     int64
				override bool
			}
			var models []candidateModel

			own := candidateModel{source: mod.Source}
			if p, ok := sealed[mod.ID]; ok {
				own.path = p
			} else {
				resolved, rerr := s.Resolve(mod.Source, mod.SHA256, nil)
				if rerr != nil {
					return rerr
				}
				own.path, own.digest = resolved.Path, resolved.Digest
				own.quant, own.params, own.size = resolved.Quant, resolved.Params, resolved.Size
			}
			models = append(models, own)

			for _, src := range extraModels {
				if src == mod.Source {
					continue // already covered by the unit's own
				}
				resolved, rerr := s.Resolve(src, "", func(done, total int64) {})
				if rerr != nil {
					return fmt.Errorf("--model %s: %w", src, rerr)
				}
				models = append(models, candidateModel{
					source: src, digest: resolved.Digest, path: resolved.Path,
					quant: resolved.Quant, params: resolved.Params, size: resolved.Size,
					override: true,
				})
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

			// One report per model, each separately saved and citable.
			var reports []*eval.Report
			for i, cm := range models {
				if len(models) > 1 {
					logf("[%d/%d] scoring against %s", i+1, len(models), cm.source)
				}
				rep, rerr := eval.Run(ctx, eval.Options{
					Suite:         suite,
					SuitePath:     suiteRecorded,
					UnitRef:       m.Ref(),
					UnitDigest:    unitDigest,
					ModelPath:     cm.path,
					ModelRef:      cm.source,
					ModelDigest:   cm.digest,
					ModelQuant:    cm.quant,
					ModelParams:   cm.params,
					ModelSize:     cm.size,
					ModelOverride: cm.override,
					System:        m.Entrypoint.SystemPrompt,
					Context:       mod.Context,
					Chat:          true,
					Repeats:       repeats,
					Devices:       devices,
					AllDevices:    allDevices,
					Prefer:        prefer,
					Backends:      backends,
					Progress:      logf,
				})
				if rerr != nil {
					return rerr
				}
				reports = append(reports, rep)
			}
			rep := reports[0]

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
				for _, r := range reports {
					if err := eval.Save(s, r); err != nil {
						logf("warning: could not save evaluation: %v", err)
					} else {
						logf("saved as %s", r.ID)
					}
				}
			}

			// One model prints the full scorecard; several print the
			// comparison, since the point of a sweep is the ranking.
			matrix := eval.NewMatrix(reports)
			if jsonOut {
				out := map[string]any{"report": rep, "reports": reports}
				if len(reports) > 1 {
					out["matrix"] = matrix
				}
				if diff != nil {
					out["diff"] = diff
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(out); err != nil {
					return err
				}
			} else if len(reports) > 1 {
				fmt.Printf("\n%s", matrix.String())
			} else {
				fmt.Printf("\n%s", rep.String())
				if diff != nil {
					fmt.Printf("\n%s", diff.String())
				}
			}

			// Exit status makes the suite usable as a gate in CI. With
			// several models it gates on the best of them, matching how
			// --all-devices gates on the best target.
			var topRate float64
			executed := false
			for _, r := range reports {
				if b := r.Best(); b != nil {
					executed = true
					if b.Rate() > topRate {
						topRate = b.Rate()
					}
				}
			}
			if !executed {
				return fmt.Errorf("no target could execute the suite")
			}
			if failUnder > 0 && topRate < failUnder {
				return fmt.Errorf("pass rate %.1f%% is below --fail-under %.1f%%", topRate, failUnder)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&suitePath, "suite", "", "suite file (default: the unit's evals/ directory)")
	cmd.Flags().StringArrayVar(&extraModels, "model", nil,
		"also score the suite against this model (repeatable) — answers whether a cheaper model would do")
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
			// The model column matters once a sweep exists: several reports
			// for one unit differ only by the model they scored.
			idW, unitW, modelW := len("ID"), len("UNIT"), len("MODEL")
			label := func(r *eval.Report) string {
				if r.ModelOverride {
					return r.Model + " (--model)"
				}
				return r.Model
			}
			for _, r := range reports {
				idW = max(idW, len(r.ID))
				unitW = max(unitW, len(r.Unit))
				modelW = max(modelW, len(label(r)))
			}
			row := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%7s  %%s\n", idW, unitW, modelW)
			fmt.Printf(row, "ID", "UNIT", "MODEL", "BEST", "TARGET")
			for _, r := range reports {
				rate, target := "—", "—"
				if best := r.Best(); best != nil {
					rate = fmt.Sprintf("%.1f%%", best.Rate())
					target = best.Label()
				}
				fmt.Printf(row, r.ID, r.Unit, label(r), rate, target)
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
			fmt.Printf("%-28s %-10s %-10s %s\n", "REF", "KIND", "SIZE", "DESCRIPTION")
			for _, r := range refs {
				// The store holds both units and workflows. Listing a
				// workflow as a unit was actively misleading: its config
				// decodes far enough to print a plausible row.
				if spec, werr := workflow.Resolve(cmd.Context(), s, r); werr == nil {
					fmt.Printf("%-28s %-10s %-10s %s\n", r, "workflow",
						fmt.Sprintf("%d agents", len(spec.Agents)), spec.Description)
					continue
				}
				m, om, err := unit.Resolve(cmd.Context(), s, r)
				if err != nil {
					fmt.Printf("%-28s (unreadable)\n", r)
					continue
				}
				var size int64
				for _, l := range om.Layers {
					size += l.Size
				}
				kind := "linked"
				if om.Annotations[unit.AnnotationSealed] == "true" {
					kind = "sealed"
				}
				fmt.Printf("%-28s %-10s %-10s %s\n", r, kind, humanSize(size), m.Description)
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
			if spec, werr := workflow.Resolve(cmd.Context(), s, args[0]); werr == nil {
				fmt.Printf("Workflow:    %s\n", spec.Ref())
				if spec.Description != "" {
					fmt.Printf("Description: %s\n", spec.Description)
				}
				fmt.Printf("State:       %s\nIsolation:   %s\n\nAgents:\n",
					spec.SharedState.Backend, spec.Network.Isolation)
				order, _ := spec.Order()
				for _, name := range order {
					a := spec.Agents[name]
					fmt.Printf("  %-16s %s", name, a.Unit)
					if a.Model != "" {
						fmt.Printf(" (model: %s)", a.Model)
					}
					fmt.Println()
				}
				return nil
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
				if !mod.Auto() {
					fmt.Printf("  %-10s %s (%s)\n", mod.ID, mod.Source, mod.Format)
					continue
				}
				// An auto entry has no source to print; what it has is a bar
				// and a shortlist, and both are what a reader wants to see
				// before running someone else's unit.
				fmt.Printf("  %-10s selected on this machine (%s)\n", mod.ID, mod.Strategy())
				if r := mod.Requirements; r != nil {
					var bars []string
					if r.MinContext > 0 {
						bars = append(bars, fmt.Sprintf("context ≥ %d", r.MinContext))
					}
					if r.MaxSizeMB > 0 {
						bars = append(bars, fmt.Sprintf("≤ %d MB", r.MaxSizeMB))
					}
					if r.MinQualityScore > 0 {
						bars = append(bars, fmt.Sprintf("eval ≥ %d cases", r.MinQualityScore))
					}
					if r.ToolCalling != "" {
						bars = append(bars, "tool calling "+r.ToolCalling)
					}
					if len(bars) > 0 {
						fmt.Printf("  %-10s requires: %s\n", "", strings.Join(bars, ", "))
					}
				}
				for _, c := range mod.Candidates {
					fmt.Printf("  %-10s   · %s\n", "", c.Source)
				}
			}
			// What an agent will reach for is the part worth reading before
			// running a stranger's unit. Names only — never a value, and
			// never whether one happens to be stored on this machine.
			if len(m.Secrets) > 0 || len(m.Config) > 0 {
				fmt.Printf("\nNeeds:\n")
				for _, sec := range m.Secrets {
					kind := "secret, optional"
					if sec.Required {
						kind = "secret, required"
					}
					dest := "env " + sec.EnvName()
					if sec.MountPath != "" {
						dest = "file " + sec.MountPath
					}
					fmt.Printf("  %-24s %-18s → %s\n", sec.Name, kind, dest)
				}
				for _, c := range m.Config {
					fmt.Printf("  %-24s %-18s → env %s\n", c.Name,
						"config = "+c.Default, c.EnvName())
				}
			}
			if len(m.Tools) > 0 {
				fmt.Printf("\nTools:\n")
				for _, t := range m.Tools {
					fmt.Printf("  %-24s %s\n", t.Name, t.Description)
					if len(t.Capabilities) > 0 {
						fmt.Printf("  %-24s   needs: %s\n", "", strings.Join(t.Capabilities, ", "))
					}
				}
			}
			if len(m.MCPServers) > 0 {
				fmt.Printf("\nMCP servers:\n")
				for _, name := range m.MCPNames() {
					srv := m.MCPServers[name]
					fmt.Printf("  %-24s %s\n", name, srv.Source)
					if len(srv.Sandbox.AllowedPaths) > 0 {
						fmt.Printf("  %-24s   may read: %s\n", "", strings.Join(srv.Sandbox.AllowedPaths, ", "))
					}
					if srv.Sandbox.Network {
						fmt.Printf("  %-24s   network: yes\n", "")
					}
				}
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
		Use:   "bench [unit]",
		Short: "Measure real throughput on every usable device",
		Long: `Bench runs the same prompt on every backend/device pair this host can
actually execute on, and reports median tokens/sec.

This is how you answer "is the NPU actually faster than the CPU here?"
empirically, instead of trusting spec sheets.

Given a unit whose model entry declares candidates, bench measures each of
them against the unit's own eval suite and reports which one this machine
would select — the same decision ` + "`nexus run`" + ` makes, without running the
agent. Results are cached per machine, so the run that follows is instant.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return benchUnit(cmd.Context(), args[0], jsonOut)
			}
			if model == "" {
				return fmt.Errorf("--model is required (a .gguf path or ollama:<name>), or name a unit to benchmark its candidates")
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
	cmd.AddCommand(newBenchCacheCmd(), newBenchExportCmd())
	return cmd
}

// benchUnit measures a unit's model candidates and reports the selection
// without running the agent.
func benchUnit(ctx context.Context, ref string, jsonOut bool) error {
	s, err := store.Open()
	if err != nil {
		return err
	}
	var m *manifest.Manifest
	srcDir := ""
	if info, serr := os.Stat(ref); serr == nil && info.IsDir() {
		if m, err = manifest.Load(ref); err != nil {
			return err
		}
		srcDir, _ = filepath.Abs(ref)
	} else {
		if m, _, err = unit.Resolve(ctx, s, ref); err != nil {
			return err
		}
		tmp, terr := os.MkdirTemp("", "nexus-bench-*")
		if terr != nil {
			return terr
		}
		defer os.RemoveAll(tmp)
		if _, err = unit.Unpack(ctx, s, ref, tmp); err != nil {
			return err
		}
		srcDir = tmp
	}

	var auto *manifest.Model
	for i := range m.Models {
		if m.Models[i].Auto() {
			auto = &m.Models[i]
			break
		}
	}
	if auto == nil {
		return fmt.Errorf(
			"%s names its model directly (%s), so there is nothing to choose between — `%s bench --model %s` measures that one model's throughput",
			m.Ref(), m.Models[0].Source, binName, m.Models[0].Source)
	}

	// Benchmarking is measurement, so the cache is written: the whole point
	// is that the next `nexus run` does not repeat this work.
	dec, err := selectAutoModel(ctx, s, m, *auto, srcDir, ref, m.Hardware.Prefer, autoModelOpts{})
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(dec)
	}
	fmt.Printf("\n%s", dec.String())
	return nil
}

func newBenchCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or clear this machine's benchmark cache",
	}
	var jsonOut bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Show cached benchmark results",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			c, err := bench.LoadCache(s)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(c)
			}
			fmt.Print(c.String())
			return nil
		},
	}
	show.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")

	clear := &cobra.Command{
		Use:   "clear",
		Short: "Discard cached benchmark results",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			if err := bench.ClearCache(s); err != nil {
				return err
			}
			fmt.Printf("Cleared %s\n", bench.CachePath(s))
			return nil
		},
	}
	cmd.AddCommand(show, clear)
	return cmd
}

func newBenchExportCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export cached benchmark results",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			c, err := bench.LoadCache(s)
			if err != nil {
				return err
			}
			switch format {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(c)
			case "csv":
				w := csv.NewWriter(os.Stdout)
				defer w.Flush()
				if err := w.Write([]string{
					"machine_id", "unit", "version", "profile", "model", "backend",
					"device", "passed", "total", "tok_per_sec", "model_size_bytes", "timestamp",
				}); err != nil {
					return err
				}
				for _, e := range c.Sorted() {
					if err := w.Write([]string{
						c.MachineID, e.Unit, e.Version, e.Profile, e.Model, e.Backend, e.Device,
						fmt.Sprint(e.Passed), fmt.Sprint(e.Total),
						fmt.Sprintf("%.3f", e.TokPerSec), fmt.Sprint(e.ModelSizeBytes),
						e.Timestamp.UTC().Format(time.RFC3339),
					}); err != nil {
						return err
					}
				}
				return w.Error()
			default:
				return fmt.Errorf("--format must be json or csv, got %q", format)
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "json", "output format: json or csv")
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
