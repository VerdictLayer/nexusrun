// Package install coordinates the dependencies a unit needs before its first
// run. It intentionally composes the existing model, MCP, secret, hardware and
// eval packages: install is a workflow, not a second implementation of them.
package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/eval"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/mcp"
	"github.com/verdictlayer/nexusrun/internal/runtimeinstall"
	"github.com/verdictlayer/nexusrun/internal/secrets"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

const (
	Ready          = "ready"
	ActionRequired = "action_required"
	Blocked        = "blocked"
	Pending        = "pending"
	Passed         = "passed"
	Failed         = "failed"
	Skipped        = "skipped"
)

type Check struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
	Detail    string `json:"detail,omitempty"`
	Action    string `json:"action,omitempty"`
	Command   string `json:"command,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type Configuration struct {
	Device            string `json:"device"`
	Backend           string `json:"backend"`
	Context           int    `json:"context"`
	MemoryRequiredMB  int64  `json:"memory_required_mb"`
	MemoryAvailableMB int64  `json:"memory_available_mb"`
	Reason            string `json:"reason"`
}

type TestCase struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}
type Tests struct {
	Suite  string     `json:"suite,omitempty"`
	Status string     `json:"status"`
	Passed int        `json:"passed"`
	Failed int        `json:"failed"`
	Cases  []TestCase `json:"cases,omitempty"`
}

type Plan struct {
	Unit          string         `json:"unit"`
	Complete      bool           `json:"complete"`
	Checks        []Check        `json:"checks"`
	Configuration *Configuration `json:"tested_configuration,omitempty"`
	Tests         *Tests         `json:"acceptance_tests,omitempty"`
}

type Options struct {
	InspectDownloadSizes bool
	Hardware             *hardware.Report
	Capabilities         []engine.Capability
}

type Request struct {
	Models   bool                                          `json:"models"`
	Runtime  bool                                          `json:"runtime"`
	Tools    bool                                          `json:"tools"`
	Secrets  map[string]string                             `json:"secrets,omitempty"`
	RunTests bool                                          `json:"run_tests"`
	Progress func(kind, message string, done, total int64) `json:"-"`
}

type loaded struct {
	m       *manifest.Manifest
	src     string
	sealed  map[string]string
	cleanup func()
}

func load(ctx context.Context, s *store.Store, ref string) (*loaded, error) {
	if info, err := os.Stat(ref); err == nil && info.IsDir() {
		m, err := manifest.Load(ref)
		if err != nil {
			return nil, err
		}
		abs, _ := filepath.Abs(ref)
		return &loaded{m: m, src: abs, sealed: map[string]string{}, cleanup: func() {}}, nil
	}
	m, _, err := unit.Resolve(ctx, s, ref)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "nexus-install-*")
	if err != nil {
		return nil, err
	}
	sealed, err := unit.Unpack(ctx, s, ref, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	return &loaded{m: m, src: tmp, sealed: sealed, cleanup: func() { os.RemoveAll(tmp) }}, nil
}

func Inspect(ctx context.Context, s *store.Store, ref string, opts Options) (*Plan, error) {
	l, err := load(ctx, s, ref)
	if err != nil {
		return nil, err
	}
	defer l.cleanup()
	return inspectLoaded(ctx, s, l, opts)
}

func inspectLoaded(ctx context.Context, s *store.Store, l *loaded, opts Options) (*Plan, error) {
	p := &Plan{Unit: l.m.Ref()}
	hw := opts.Hardware
	if hw == nil {
		hw = hardware.Detect()
	}
	caps := opts.Capabilities
	if caps == nil {
		caps = engine.ProbeAll()
	}

	var largest int64
	for _, mod := range l.m.Models {
		targets := []struct {
			id, source, sha string
			size            int64
		}{{mod.ID, mod.Source, mod.SHA256, mod.SizeBytes}}
		if mod.Auto() {
			targets = nil
			for i, c := range mod.Candidates {
				targets = append(targets, struct {
					id, source, sha string
					size            int64
				}{fmt.Sprintf("%s.%d", mod.ID, i), c.Source, c.SHA256, c.SizeBytes})
			}
		}
		for _, t := range targets {
			sz := t.size
			resolvedSource := sourceForStore(t.source, l.src)
			if path, ok := l.sealed[t.id]; ok {
				if st, e := os.Stat(path); e == nil {
					sz = st.Size()
				}
				p.Checks = append(p.Checks, Check{ID: "model:" + t.id, Kind: "model", Status: Ready, Summary: t.source + " is included", SizeBytes: sz})
				largest = max(largest, sz)
				continue
			}
			if cached, ok := s.Cached(resolvedSource, t.sha); ok {
				sz = cached.Size
				p.Checks = append(p.Checks, Check{ID: "model:" + t.id, Kind: "model", Status: Ready, Summary: t.source + " is available", SizeBytes: sz})
				largest = max(largest, sz)
				continue
			}
			if sz == 0 && opts.InspectDownloadSizes {
				if remote, e := s.DownloadSize(ctx, resolvedSource); e == nil {
					sz = remote
				}
			}
			detail := "The model is not present on this machine."
			if sz == 0 {
				detail += " Its origin did not advertise a download size; declare size_bytes to make approval deterministic."
			}
			p.Checks = append(p.Checks, Check{ID: "model:" + t.id, Kind: "model", Status: ActionRequired, Summary: "Download " + t.source, Detail: detail, Action: "download_model", SizeBytes: sz})
			largest = max(largest, sz)
		}
	}

	if l.m.Entrypoint.Type == "chat" {
		available := false
		needTools := len(l.m.Tools) > 0 || len(l.m.MCPServers) > 0
		for _, c := range caps {
			if c.Available && c.AcceptsModelPath && (!needTools || c.SupportsTools) {
				available = true
				break
			}
		}
		if available {
			p.Checks = append(p.Checks, Check{ID: "runtime", Kind: "runtime", Status: Ready, Summary: "Compatible inference runtime is installed"})
		} else if rel, ok := runtimeinstall.Recommended(hw.OS, hw.Arch); ok {
			sz := rel.SizeBytes
			if sz == 0 && opts.InspectDownloadSizes {
				sz = rel.InspectSize(ctx)
			}
			detail := fmt.Sprintf("No installed runtime can execute this %s unit. NexusRun tested %s %s for %s/%s.", map[bool]string{true: "tool-using", false: "chat"}[needTools], rel.Name, rel.Version, hw.OS, hw.Arch)
			p.Checks = append(p.Checks, Check{ID: "runtime", Kind: "runtime", Status: ActionRequired, Summary: fmt.Sprintf("Install %s %s", rel.Name, rel.Version), Detail: detail, Action: "install_runtime", SizeBytes: sz})
		} else {
			p.Checks = append(p.Checks, Check{ID: "runtime", Kind: "runtime", Status: Blocked, Summary: "No tested runtime package for this platform", Detail: fmt.Sprintf("Install llama.cpp manually for %s/%s and rerun installation.", hw.OS, hw.Arch)})
		}
	}

	p.Configuration = configuration(l.m, hw, caps, largest)
	if p.Configuration != nil && p.Configuration.MemoryRequiredMB == 0 {
		p.Checks = append(p.Checks, Check{ID: "hardware", Kind: "hardware", Status: ActionRequired, Summary: "Memory fit will be checked after the model size is known", Detail: p.Configuration.Reason, Action: "download_model"})
	} else if p.Configuration != nil && p.Configuration.MemoryRequiredMB > p.Configuration.MemoryAvailableMB {
		p.Checks = append(p.Checks, Check{ID: "hardware", Kind: "hardware", Status: Blocked, Summary: "Not enough memory for the declared model", Detail: p.Configuration.Reason, Action: "use_tested_configuration"})
	} else if p.Configuration != nil {
		p.Checks = append(p.Checks, Check{ID: "hardware", Kind: "hardware", Status: Ready, Summary: "Tested configuration fits", Detail: p.Configuration.Reason, Command: fmt.Sprintf("nexus run %s --device %s", l.m.Ref(), p.Configuration.Device)})
	}

	for _, name := range l.m.MCPNames() {
		src, e := mcp.ParseSource(l.m.MCPServers[name].Source)
		if e != nil {
			p.Checks = append(p.Checks, Check{ID: "tool:" + name, Kind: "tool", Status: Blocked, Summary: name + " has an invalid source", Detail: e.Error()})
			continue
		}
		if src.Installed(s) {
			p.Checks = append(p.Checks, Check{ID: "tool:" + name, Kind: "tool", Status: Ready, Summary: name + " is installed"})
		} else {
			pin := "pinned"
			if !src.Pinned() {
				pin = "not pinned"
			}
			p.Checks = append(p.Checks, Check{ID: "tool:" + name, Kind: "tool", Status: ActionRequired, Summary: "Install tool " + name, Detail: l.m.MCPServers[name].Source + " (" + pin + ")", Action: "install_tools", Command: "nexus tools install " + l.m.Ref()})
		}
	}

	secretStore, e := secrets.Open(s)
	if e != nil {
		return nil, e
	}
	present := map[string]bool{}
	for _, x := range secretStore.List(l.m.Name) {
		if x.Device == "" && !x.Expired() {
			present[x.Key] = true
		}
	}
	for _, sec := range l.m.Secrets {
		status := Ready
		summary := sec.Name + " is configured"
		action := ""
		command := ""
		if !present[sec.Name] {
			status = ActionRequired
			summary = "Configure secret " + sec.Name
			action = "set_secret"
			command = fmt.Sprintf("nexus secret set %s %s --stdin", l.m.Name, sec.Name)
			if !sec.Required {
				status = Skipped
				summary = "Optional secret " + sec.Name + " is not configured"
			}
		}
		p.Checks = append(p.Checks, Check{ID: "secret:" + sec.Name, Kind: "secret", Status: status, Summary: summary, Action: action, Command: command, Detail: "Values are stored in the existing encrypted NexusRun secret store."})
	}

	if suitePath, e := eval.Discover(l.src); e == nil {
		suite, le := eval.Load(suitePath)
		if le != nil {
			return nil, le
		}
		p.Tests = &Tests{Suite: suite.Name, Status: Pending, Cases: make([]TestCase, 0, len(suite.Cases))}
		for _, c := range suite.Cases {
			p.Tests.Cases = append(p.Tests.Cases, TestCase{Name: c.Name, Status: Pending})
		}
	} else {
		p.Tests = &Tests{Status: Skipped}
	}
	p.Complete = complete(p)
	return p, nil
}

func complete(p *Plan) bool {
	for _, c := range p.Checks {
		if c.Status == ActionRequired || c.Status == Blocked {
			return false
		}
	}
	return p.Tests == nil || p.Tests.Status == Passed || p.Tests.Status == Skipped
}

func configuration(m *manifest.Manifest, hw *hardware.Report, caps []engine.Capability, modelBytes int64) *Configuration {
	if m.Entrypoint.Type != "chat" {
		return nil
	}
	context := contextFor(m)
	if modelBytes == 0 && m.Hardware.MinMemoryGB == 0 {
		return &Configuration{Device: "cpu", Backend: "llama.cpp", Context: context, MemoryAvailableMB: hw.TotalRAMMB, Reason: "The model origin did not provide a size. NexusRun will measure the downloaded file, then re-run the GPU and system-memory fit check before acceptance tests."}
	}
	required := int64(float64(modelBytes)/(1024*1024)*1.20) + 512
	if min := int64(m.Hardware.MinMemoryGB * 1024); min > required {
		required = min
	}
	usable := map[string]string{}
	for _, c := range caps {
		if c.Available && c.AcceptsModelPath {
			for _, d := range c.Devices {
				if _, ok := usable[d]; !ok {
					usable[d] = c.Backend
				}
			}
		}
	}
	var rejected []string
	for _, want := range m.Hardware.Prefer {
		backend, driven := usable[want]
		if !hw.Has(want) {
			if want != hardware.ClassCPU {
				rejected = append(rejected, strings.ToUpper(want)+" is unavailable")
			}
			continue
		}
		if !driven {
			rejected = append(rejected, strings.ToUpper(want)+" has no compatible installed runtime")
			continue
		}
		if driven {
			avail := hw.TotalRAMMB
			if want == hardware.ClassGPU {
				for _, d := range hw.Devices {
					if d.Class == want && d.MemoryMB > 0 {
						avail = d.MemoryMB
						break
					}
				}
			}
			if avail >= required {
				reason := fmt.Sprintf("%s/%s has %d MB available; this model needs about %d MB including overhead.", backend, want, avail, required)
				if len(rejected) > 0 {
					reason = strings.Join(rejected, "; ") + ". " + reason
				}
				return &Configuration{Device: want, Backend: backend, Context: context, MemoryRequiredMB: required, MemoryAvailableMB: avail, Reason: reason}
			}
			rejected = append(rejected, fmt.Sprintf("%s has %d MB but needs about %d MB", strings.ToUpper(want), avail, required))
		}
	}
	// The pinned CPU runtime is the tested fallback when a GPU is absent or
	// too small. Unified-memory machines naturally use total RAM here.
	reason := fmt.Sprintf("Use llama.cpp/CPU with context %d: the preferred GPU is unavailable or too small. CPU execution has %d MB system RAM; about %d MB is required.", context, hw.TotalRAMMB, required)
	if len(rejected) > 0 {
		reason = strings.Join(rejected, "; ") + ". " + reason
	}
	return &Configuration{Device: "cpu", Backend: "llama.cpp", Context: context, MemoryRequiredMB: required, MemoryAvailableMB: hw.TotalRAMMB, Reason: reason}
}

func contextFor(m *manifest.Manifest) int {
	if len(m.Models) > 0 && m.Models[0].Context > 0 {
		return m.Models[0].Context
	}
	return 4096
}

func Apply(ctx context.Context, s *store.Store, ref string, req Request) (*Plan, error) {
	l, err := load(ctx, s, ref)
	if err != nil {
		return nil, err
	}
	defer l.cleanup()
	emit := func(k, m string, d, t int64) {
		if req.Progress != nil {
			req.Progress(k, m, d, t)
		}
	}
	if req.Runtime && l.m.Entrypoint.Type == "chat" {
		if rel, ok := runtimeinstall.HostRecommendation(); ok {
			emit("runtime", "Downloading "+rel.Name+" "+rel.Version, 0, rel.SizeBytes)
			if err := runtimeinstall.Install(ctx, s, rel, func(d, t int64) { emit("runtime", "Downloading "+rel.Name+" "+rel.Version, d, t) }); err != nil {
				return nil, err
			}
		}
	}
	if req.Models {
		for _, mod := range l.m.Models {
			targets := []struct{ source, sha string }{{mod.Source, mod.SHA256}}
			if mod.Auto() {
				targets = nil
				for _, c := range mod.Candidates {
					targets = append(targets, struct{ source, sha string }{c.Source, c.SHA256})
				}
			}
			for _, t := range targets {
				resolvedSource := sourceForStore(t.source, l.src)
				if _, ok := s.Cached(resolvedSource, t.sha); ok {
					continue
				}
				emit("model", "Downloading "+t.source, 0, 0)
				if strings.HasPrefix(t.source, "ollama:") {
					bin, e := exec.LookPath("ollama")
					if e != nil {
						return nil, fmt.Errorf("%s needs Ollama to download it; install the pinned Ollama runtime, then rerun: ollama pull %s", t.source, strings.TrimPrefix(t.source, "ollama:"))
					}
					c := exec.CommandContext(ctx, bin, "pull", strings.TrimPrefix(t.source, "ollama:"))
					c.Stdout = os.Stdout
					c.Stderr = os.Stderr
					if e = c.Run(); e != nil {
						return nil, e
					}
				} else {
					_, e := s.Resolve(resolvedSource, t.sha, func(d, total int64) { emit("model", "Downloading "+t.source, d, total) })
					if e != nil {
						return nil, e
					}
				}
			}
		}
	}
	if req.Tools {
		for _, name := range l.m.MCPNames() {
			src, e := mcp.ParseSource(l.m.MCPServers[name].Source)
			if e != nil {
				return nil, e
			}
			emit("tool", "Installing "+name, 0, 0)
			if e = src.Install(ctx, s, false, func(f string, a ...any) { emit("tool", fmt.Sprintf(f, a...), 0, 0) }); e != nil {
				return nil, e
			}
		}
	}
	if len(req.Secrets) > 0 {
		st, e := secrets.Open(s)
		if e != nil {
			return nil, e
		}
		keys := make([]string, 0, len(req.Secrets))
		declared := map[string]bool{}
		for _, sec := range l.m.Secrets {
			declared[sec.Name] = true
		}
		for k := range req.Secrets {
			if !declared[k] {
				return nil, fmt.Errorf("secret %s is not declared by %s", k, l.m.Ref())
			}
			if req.Secrets[k] == "" {
				return nil, fmt.Errorf("secret %s cannot be empty", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if e = st.Set(l.m.Name, k, req.Secrets[k], secrets.SetOptions{}); e != nil {
				return nil, e
			}
			emit("secret", "Stored "+k, 1, 1)
		}
	}
	p, e := inspectLoaded(ctx, s, l, Options{InspectDownloadSizes: false})
	if e != nil {
		return nil, e
	}
	if !req.RunTests && p.Tests != nil && p.Tests.Status == Pending {
		p.Tests.Status = Skipped
		p.Complete = complete(p)
	}
	if req.RunTests && p.Tests != nil && p.Tests.Status == Pending && readyForTests(p) {
		tests, e := runTests(ctx, s, l, p.Configuration, emit)
		if e != nil {
			p.Tests = &Tests{Status: Failed, Cases: []TestCase{{Name: "setup", Status: Failed, Detail: e.Error()}}, Failed: 1}
		} else {
			p.Tests = tests
		}
		p.Complete = complete(p)
	}
	return p, nil
}

func readyForTests(p *Plan) bool {
	for _, c := range p.Checks {
		if c.Status == ActionRequired || c.Status == Blocked {
			return false
		}
	}
	return true
}

func runTests(ctx context.Context, s *store.Store, l *loaded, config *Configuration, emit func(string, string, int64, int64)) (*Tests, error) {
	path, err := eval.Discover(l.src)
	if err != nil {
		return &Tests{Status: Skipped}, nil
	}
	suite, err := eval.Load(path)
	if err != nil {
		return nil, err
	}
	if len(l.m.Models) == 0 {
		return nil, fmt.Errorf("acceptance tests require a model")
	}
	mod := l.m.Models[0]
	source, sha, id, context := mod.Source, mod.SHA256, mod.ID, mod.Context
	if mod.Auto() {
		source, sha = mod.Candidates[0].Source, mod.Candidates[0].SHA256
		id = mod.ID + ".0"
		if mod.Candidates[0].Context > 0 {
			context = mod.Candidates[0].Context
		}
	}
	modelPath := l.sealed[id]
	var rm *store.ResolvedModel
	if modelPath == "" {
		rm, err = s.Resolve(sourceForStore(source, l.src), sha, nil)
		if err != nil {
			return nil, err
		}
		modelPath = rm.Path
	}
	emit("tests", "Running "+suite.Name, 0, int64(len(suite.Cases)))
	devices := []string(nil)
	prefer := l.m.Hardware.Prefer
	if config != nil && config.Device != "" {
		devices = []string{config.Device}
		prefer = devices
	}
	rep, err := eval.Run(ctx, eval.Options{Suite: suite, SuitePath: path, UnitRef: l.m.Ref(), ModelPath: modelPath, ModelRef: source, System: l.m.Entrypoint.SystemPrompt, Context: context, Chat: true, Devices: devices, Prefer: prefer, Progress: func(f string, a ...any) { emit("tests", fmt.Sprintf(f, a...), 0, int64(len(suite.Cases))) }})
	if err != nil {
		return nil, err
	}
	_ = eval.Save(s, rep)
	out := &Tests{Suite: suite.Name, Status: Passed}
	best := rep.Best()
	if best == nil {
		return nil, fmt.Errorf("no target could execute acceptance tests")
	}
	for _, c := range best.Cases {
		tc := TestCase{Name: c.Name, Status: c.Status()}
		if c.Error != "" {
			tc.Detail = c.Error
		} else if len(c.Failures) > 0 {
			f := c.Failures[0]
			tc.Detail = f.Kind + " " + f.Want
			if f.Detail != "" {
				tc.Detail += ": " + f.Detail
			}
		}
		out.Cases = append(out.Cases, tc)
		if c.Passed() {
			out.Passed++
		} else {
			out.Failed++
		}
	}
	if out.Failed > 0 {
		out.Status = Failed
	}
	emit("tests", fmt.Sprintf("%d passed, %d failed", out.Passed, out.Failed), int64(len(best.Cases)), int64(len(best.Cases)))
	return out, nil
}

func sourceForStore(source, unitDir string) string {
	if source == "" || filepath.IsAbs(source) || strings.Contains(source, "://") || strings.HasPrefix(source, "hf:") || strings.HasPrefix(source, "ollama:") {
		return source
	}
	return filepath.Join(unitDir, filepath.FromSlash(source))
}
