package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/verdictlayer/nexusrun/internal/agent"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/eval"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/runtimepkg"
	"github.com/verdictlayer/nexusrun/internal/secrets"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

func HostHardware() Hardware {
	h := hardware.Detect()
	return Hardware{h.OS, h.Arch, h.CPUCores, h.TotalRAMMB}
}

func pinnedRuntime(home string, r Release) (*engine.LlamaServer, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, errors.New("fleet execution requires Linux amd64")
	}
	path, m, err := runtimepkg.Resolve(home, r.RuntimeID)
	if err != nil {
		return nil, err
	}
	if _, err = runtimepkg.VerifyDirectory(path); err != nil {
		return nil, err
	}
	if m.Accelerator != "cpu" || runtimepkg.HostCompatible(*m) != nil {
		return nil, errors.New("fleet runtime must match the Linux amd64 CPU profile")
	}
	features := strings.Join(hardware.Detect().CPUFeatures, " ")
	for _, f := range m.CPUFeatures {
		if !strings.Contains(" "+features+" ", " "+f+" ") {
			return nil, errors.New("required runtime CPU feature unavailable")
		}
	}
	var libraries []string
	for _, p := range m.LibraryPaths {
		libraries = append(libraries, filepath.Join(path, filepath.FromSlash(p)))
	}
	return engine.NewPinnedServer(filepath.Join(path, filepath.FromSlash(m.Executable)), libraries), nil
}

// modelLayer enforces the fleet execution profile: one sealed model, a chat
// entrypoint, and only what runs from the artifact itself. Script tools and
// secrets qualify; MCP servers do not, because they are fetched and started
// from outside the artifact.
func modelLayer(m *manifest.Manifest, om ocispec.Manifest) (ocispec.Descriptor, error) {
	if len(m.Models) != 1 || m.Models[0].Auto() || m.Entrypoint.Type != "chat" || len(m.MCPServers) > 0 {
		return ocispec.Descriptor{}, errors.New("fleet release must be a sealed single-model chat unit without MCP servers")
	}
	for _, t := range m.Tools {
		if t.Exec.Type != manifest.ToolExecScript {
			return ocispec.Descriptor{}, errors.New("fleet release tools must be script tools shipped in the unit")
		}
	}
	var model ocispec.Descriptor
	count := 0
	for _, layer := range om.Layers {
		if layer.MediaType == unit.MediaTypeModelGGUF {
			model = layer
			count++
		}
	}
	if om.Annotations[unit.AnnotationSealed] != "true" || count != 1 || model.Annotations[unit.AnnotationModelID] != m.Models[0].ID {
		return ocispec.Descriptor{}, errors.New("fleet release requires exactly one matching sealed model layer")
	}
	return model, nil
}

// codeError carries the failure code a host reports for a rejected candidate.
type codeError struct {
	code string
	err  error
}

func (e *codeError) Error() string { return e.err.Error() }

func failure(code string, err error) error { return &codeError{code, err} }

// Code is the report code for an error from OpenAgent or preparation.
func Code(err error) string {
	var c *codeError
	if errors.As(err, &c) {
		return c.code
	}
	return "artifact"
}

// Agent is one verified release, ready to answer. Its tools run from a private
// copy of the release's own files, under the unit's sandbox, with the secrets
// this host holds for the unit. Nothing is fetched or installed.
type Agent struct {
	Release  Release
	Manifest *manifest.Manifest

	backend *engine.LlamaServer
	model   string
	dir     string
	inj     *secrets.Injection
	tools   []engine.ToolDef
	scripts *agent.ScriptTools
}

// OpenAgent verifies a downloaded release against its digest and readies it.
func OpenAgent(ctx context.Context, home string, r Release) (*Agent, error) {
	backend, err := pinnedRuntime(home, r)
	if err != nil {
		return nil, failure("runtime", errors.New("pinned fleet runtime is unavailable or corrupt"))
	}
	a := &Agent{Release: r, backend: backend}
	if err = a.open(ctx, home); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func (a *Agent) open(ctx context.Context, home string) error {
	r := a.Release
	local := &store.Store{Root: ReleaseHome(home, r)}
	corrupt := failure("artifact", errors.New("fleet artifact is unavailable or corrupt"))
	if err := unit.Verify(ctx, local, r.Digest); err != nil {
		return corrupt
	}
	m, om, err := unit.Resolve(ctx, local, r.Digest)
	if err != nil {
		return corrupt
	}
	layer, err := modelLayer(m, om)
	if err != nil {
		return failure("artifact", err)
	}
	a.Manifest, a.model = m, local.BlobPath(layer.Digest.String())
	// A fresh copy per agent: a tool can write to its own directory, and what
	// one run left behind must not change what the next one starts from.
	if a.dir, err = os.MkdirTemp(local.Root, ".run-*"); err != nil {
		return failure("storage", err)
	}
	if _, err = unit.Unpack(ctx, local, r.Digest, a.dir); err != nil {
		return corrupt
	}
	if len(m.Secrets) > 0 || len(m.Config) > 0 {
		st, err := secrets.Open(&store.Store{Root: home})
		if err != nil {
			return failure("secrets", err)
		}
		if a.inj, err = st.Inject(m, secrets.InjectOptions{Device: os.Getenv("NEXUS_DEVICE_ID")}); err != nil {
			return failure("secrets", err)
		}
	}
	if len(m.Tools) > 0 {
		self, err := os.Executable()
		if err != nil {
			return failure("runtime", err)
		}
		userHome, _ := os.UserHomeDir()
		var env []string
		if a.inj != nil {
			env = a.inj.Env
		}
		a.tools = agent.ToolDefs(m.Tools)
		a.scripts = agent.NewScriptTools(m, agent.ScriptOptions{WorkDir: a.dir, HomeDir: userHome, SelfExe: self, Env: env})
	}
	return nil
}

// Dir is the unpacked release.
func (a *Agent) Dir() string { return a.dir }

// Close stops the model server and removes the unpacked copy and any secret
// files written for it.
func (a *Agent) Close() {
	a.backend.Shutdown()
	if a.inj != nil {
		a.inj.Close()
	}
	if a.dir != "" {
		_ = os.RemoveAll(a.dir)
	}
}

func (a *Agent) temperature() float64 {
	if t := a.Manifest.Models[0].Temperature; t != nil {
		return *t
	}
	return 0.7
}

// run answers one request through the agent loop, executing the release's
// tools when the model calls them.
func (a *Agent) run(ctx context.Context, req engine.Request, stream func(string)) (*agent.Result, error) {
	req.ModelPath, req.ModelRef = a.model, a.Manifest.Models[0].Source
	req.Context, req.Device, req.Chat = a.Manifest.Models[0].Context, "cpu", true
	opts := agent.Options{Backend: a.backend, Request: req, Stream: stream}
	if a.scripts != nil {
		opts.Tools, opts.Executors = a.tools, []agent.Executor{a.scripts}
	}
	return agent.Run(ctx, opts)
}

// Chat continues a conversation. The release's own system prompt comes
// first; a caller's system messages are added to it, never put in its place.
func (a *Agent) Chat(ctx context.Context, msgs []engine.Message, maxTokens int, stream func(string)) (*agent.Result, error) {
	system := a.Manifest.Entrypoint.SystemPrompt
	var rest []engine.Message
	for _, m := range msgs {
		if m.Role == engine.RoleSystem {
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
			continue
		}
		rest = append(rest, m)
	}
	var conv []engine.Message
	if system != "" {
		conv = append(conv, engine.Message{Role: engine.RoleSystem, Content: system})
	}
	conv = append(conv, rest...)
	return a.run(ctx, engine.Request{Messages: conv, MaxTokens: maxTokens, Temperature: a.temperature()}, stream)
}

// generate is the eval hook: each case is answered the way a request would be.
func (a *Agent) generate(ctx context.Context, _ engine.Backend, req engine.Request) (*engine.Result, error) {
	res, err := a.run(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	return &engine.Result{Text: res.Text, TokensOut: res.TokensOut, EvalTPS: res.EvalTPS, Backend: res.Backend, Device: res.Device}, nil
}

func checkCandidate(ctx context.Context, home string, r Release) error {
	s := &store.Store{Root: ReleaseHome(home, r)}
	if err := unit.Verify(ctx, s, r.Digest); err != nil {
		return err
	}
	digest, err := unit.Digest(ctx, s, r.Digest)
	if err != nil || digest != r.Digest {
		return errors.New("local fleet artifact integrity failure")
	}
	return unit.ValidateFleetAgent(ctx, s, r.Digest, "linux/amd64/cpu")
}

func NativePrepare(home string) Prepare {
	return func(ctx context.Context, e Enrollment, r Release) Outcome {
		fail := func(code string, err error) Outcome { return Outcome{Code: code, Detail: err.Error()} }
		// Check the runtime before downloading anything it could not run.
		if _, err := pinnedRuntime(home, r); err != nil {
			return fail("runtime", err)
		}
		if err := os.MkdirAll(ReleaseHome(home, r), 0o700); err != nil {
			return fail("storage", err)
		}
		s, err := store.OpenRoot(ReleaseHome(home, r))
		if err != nil {
			return fail("storage", err)
		}
		if err = unit.PullPinned(ctx, s, e.Repository, r.Digest, e.Token); err != nil {
			return fail("artifact", err)
		}
		if err = checkCandidate(ctx, home, r); err != nil {
			return fail("artifact", err)
		}
		a, err := OpenAgent(ctx, home, r)
		if err != nil {
			return fail(Code(err), err)
		}
		defer a.Close()
		suitePath, err := eval.Discover(a.Dir())
		if err != nil {
			return fail("evaluation", err)
		}
		raw, err := os.ReadFile(suitePath)
		if err != nil {
			return fail("evaluation", err)
		}
		suite, err := eval.Parse(raw)
		if err != nil {
			return fail("evaluation", err)
		}
		m := a.Manifest
		report, err := eval.Run(ctx, eval.Options{Suite: suite, UnitRef: m.Ref(), UnitDigest: r.Digest, ModelPath: a.model, ModelRef: m.Models[0].Source, System: m.Entrypoint.SystemPrompt, Context: m.Models[0].Context, Chat: true, Repeats: 1, Devices: []string{"cpu"}, Prefer: []string{"cpu"}, Backends: []engine.Backend{a.backend}, Generate: a.generate})
		if err != nil {
			return fail("evaluation", err)
		}
		if len(report.Targets) != 1 {
			return fail("evaluation", errors.New("acceptance tests did not run on the CPU runtime"))
		}
		target := report.Targets[0]
		sum := sha256.Sum256(raw)
		outcome := &SuiteOutcome{Digest: "sha256:" + hex.EncodeToString(sum[:])}
		var failed []string
		for _, c := range target.Cases {
			outcome.Cases = append(outcome.Cases, CaseOutcome{c.Name, c.Passed()})
			if !c.Passed() {
				failed = append(failed, c.Name)
			}
		}
		out := Outcome{Passed: target.Passed, Total: target.Total, Code: "ok", Suite: outcome}
		switch {
		case target.Error != "":
			out.Code, out.Detail = "evaluation", target.Error
		case target.Total == 0:
			out.Code, out.Detail = "evaluation", "the acceptance suite has no tests"
		case target.Passed != target.Total:
			out.Code, out.Detail = "evaluation", "failed: "+strings.Join(failed, ", ")
		}
		return out
	}
}

// Run answers one prompt with the active release. Updates affect subsequent
// invocations; an invocation already using an older release keeps it.
func Run(ctx context.Context, home, prompt string, maxTokens int) (*agent.Result, error) {
	unlock, err := Lock(home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	a, err := openActive(ctx, home)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	return a.Chat(ctx, []engine.Message{{Role: engine.RoleUser, Content: prompt}}, maxTokens, nil)
}

var errNoActive = errors.New("no active fleet release; run nexus fleet sync")

// activeRelease is the release this host has activated, checked against its
// enrollment.
func activeRelease(home string) (*Release, error) {
	e, err := LoadEnrollment(home)
	if err != nil {
		return nil, err
	}
	s, err := LoadState(home)
	if err != nil {
		return nil, err
	}
	if s.Active == nil {
		return nil, errNoActive
	}
	r := s.Active.Release
	c, err := NewClient(e.Repository, e.Token)
	if err != nil {
		return nil, err
	}
	if err = r.Validate(c.Repository, e.Channel); err != nil {
		return nil, err
	}
	return &r, nil
}

func openActive(ctx context.Context, home string) (*Agent, error) {
	r, err := activeRelease(home)
	if err != nil {
		return nil, err
	}
	return OpenAgent(ctx, home, *r)
}
