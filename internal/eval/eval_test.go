package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/engine"
)

// stubBackend stands in for a real execution backend. Injecting backends
// is what makes this package testable at all: the alternative is a test
// that needs llama.cpp, a GGUF file, and specific silicon.
type stubBackend struct {
	name  string
	cap   engine.Capability
	gen   func(req engine.Request, call int) (*engine.Result, error)
	calls int
}

func (s *stubBackend) Name() string { return s.name }

func (s *stubBackend) Probe() engine.Capability {
	c := s.cap
	c.Backend = s.name
	return c
}

func (s *stubBackend) Generate(_ context.Context, req engine.Request) (*engine.Result, error) {
	s.calls++
	return s.gen(req, s.calls)
}

// cpuStub answers every prompt with the same text, on CPU. CPU is the one
// device class present on every machine, so tests built on it do not
// depend on the hardware they run on.
func cpuStub(name, reply string) *stubBackend {
	return &stubBackend{
		name: name,
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}},
		gen: func(engine.Request, int) (*engine.Result, error) {
			return &engine.Result{Text: reply, EvalTPS: 10}, nil
		},
	}
}

func suiteOf(cases ...Case) *Suite {
	return &Suite{APIVersion: APIVersion, Kind: Kind, Name: "test-suite", Cases: cases}
}

func passCase(name, needle string) Case {
	return Case{Name: name, Prompt: "prompt for " + name, Expect: Expect{Contains: []string{needle}}}
}

func baseOpts(s *Suite, backends ...engine.Backend) Options {
	return Options{
		Suite:     s,
		UnitRef:   "test-unit:0.1.0",
		ModelPath: "/models/test-Q4_K_M.gguf",
		ModelRef:  "hf:org/repo/test-Q4_K_M.gguf",
		Prefer:    []string{"npu", "gpu", "cpu"},
		Backends:  backends,
	}
}

func TestRunScoresEachCase(t *testing.T) {
	s := suiteOf(
		passCase("finds-code", "E_TIMEOUT"),
		passCase("finds-nothing", "NOT_IN_OUTPUT"),
	)
	rep, err := Run(context.Background(), baseOpts(s, cpuStub("stub", "the code is E_TIMEOUT")))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(rep.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(rep.Targets))
	}
	tr := rep.Targets[0]
	if tr.Passed != 1 || tr.Total != 2 {
		t.Errorf("passed %d/%d, want 1/2", tr.Passed, tr.Total)
	}
	if got := tr.Rate(); got != 50 {
		t.Errorf("Rate() = %.1f, want 50", got)
	}
	if tr.Cases[0].Status() != "pass" || tr.Cases[1].Status() != "fail" {
		t.Errorf("statuses = %q, %q; want pass, fail", tr.Cases[0].Status(), tr.Cases[1].Status())
	}
	// The failing case must carry the assertion that broke and what the
	// model actually said — without both, the report cannot be acted on.
	if len(tr.Cases[1].Failures) == 0 {
		t.Error("failing case recorded no failed assertions")
	}
	if tr.Cases[1].Output == "" {
		t.Error("failing case recorded no sample output")
	}
	// Provenance: a score is only meaningful next to what produced it.
	if rep.ModelQuant != "Q4_K_M" {
		t.Errorf("ModelQuant = %q, want Q4_K_M", rep.ModelQuant)
	}
	if rep.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 by default", rep.Temperature)
	}
	if rep.ID == "" || rep.Host == nil {
		t.Error("report is missing an ID or host record")
	}
}

// A case that passes sometimes is not a passing case. Averaging repeats
// away is how unreliable agents reach production looking fine.
func TestRepeatsSurfaceFlakinessAsFailure(t *testing.T) {
	flaky := &stubBackend{
		name: "flaky",
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}},
		gen: func(_ engine.Request, call int) (*engine.Result, error) {
			if call%2 == 1 {
				return &engine.Result{Text: "E_TIMEOUT", EvalTPS: 10}, nil
			}
			return &engine.Result{Text: "I cannot help", EvalTPS: 10}, nil
		},
	}
	opts := baseOpts(suiteOf(passCase("unstable", "E_TIMEOUT")), flaky)
	opts.Repeats = 2

	rep, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	tr := rep.Targets[0]
	c := tr.Cases[0]
	if !c.Flaky {
		t.Error("case not marked flaky despite disagreeing repeats")
	}
	if c.Passed() {
		t.Error("a flaky case must not count as passed")
	}
	if c.Status() != "flaky" {
		t.Errorf("Status() = %q, want flaky", c.Status())
	}
	if tr.Passed != 0 || tr.Flaky != 1 {
		t.Errorf("target passed=%d flaky=%d, want 0 and 1", tr.Passed, tr.Flaky)
	}
	if flaky.calls != 2 {
		t.Errorf("backend called %d times, want 2", flaky.calls)
	}
}

// A backend that cannot execute the model has produced no evidence about
// the unit. Reporting that as "every case failed" would blame the unit
// for the host's missing CUDA build.
func TestBackendThatCannotExecuteIsReportedSeparately(t *testing.T) {
	broken := &stubBackend{
		name: "broken",
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}},
		gen: func(engine.Request, int) (*engine.Result, error) {
			return nil, errors.New("no CUDA backend compiled in")
		},
	}
	rep, err := Run(context.Background(), baseOpts(suiteOf(
		passCase("a", "x"), passCase("b", "y"), passCase("c", "z"),
	), broken))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	tr := rep.Targets[0]
	if tr.Error == "" {
		t.Fatal("target error not set for a backend that cannot generate")
	}
	if len(tr.Cases) != 0 {
		t.Errorf("recorded %d cases for an unusable target, want 0", len(tr.Cases))
	}
	// Fail fast: the remaining cases must not be attempted.
	if broken.calls != 1 {
		t.Errorf("backend called %d times, want 1", broken.calls)
	}
	if rep.Best() != nil {
		t.Error("Best() returned an unusable target")
	}
}

// An error partway through is that case's problem, not the target's.
func TestMidSuiteErrorFailsOnlyThatCase(t *testing.T) {
	partial := &stubBackend{
		name: "partial",
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}},
		gen: func(_ engine.Request, call int) (*engine.Result, error) {
			if call == 2 {
				return nil, errors.New("context window exceeded")
			}
			return &engine.Result{Text: "ok", EvalTPS: 10}, nil
		},
	}
	rep, err := Run(context.Background(), baseOpts(suiteOf(
		passCase("first", "ok"), passCase("second", "ok"), passCase("third", "ok"),
	), partial))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	tr := rep.Targets[0]
	if tr.Error != "" {
		t.Errorf("target marked unusable (%q) for a single failing case", tr.Error)
	}
	if tr.Total != 3 {
		t.Fatalf("evaluated %d cases, want 3", tr.Total)
	}
	if tr.Cases[1].Status() != "error" {
		t.Errorf("second case status = %q, want error", tr.Cases[1].Status())
	}
	if tr.Passed != 2 {
		t.Errorf("passed = %d, want 2", tr.Passed)
	}
}

func TestAllDevicesEvaluatesEveryPairAndDefaultPicksOne(t *testing.T) {
	s := suiteOf(passCase("only", "ok"))

	// Two distinct backends both usable on CPU are two real targets.
	optsAll := baseOpts(s, cpuStub("alpha", "ok"), cpuStub("beta", "ok"))
	optsAll.AllDevices = true
	repAll, err := Run(context.Background(), optsAll)
	if err != nil {
		t.Fatalf("Run(--all-devices) error = %v", err)
	}
	if len(repAll.Targets) != 2 {
		t.Fatalf("got %d targets with AllDevices, want 2", len(repAll.Targets))
	}

	// Without it, only where the unit would actually run.
	repOne, err := Run(context.Background(), baseOpts(s, cpuStub("alpha", "ok"), cpuStub("beta", "ok")))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(repOne.Targets) != 1 {
		t.Fatalf("got %d targets by default, want 1", len(repOne.Targets))
	}
	if repOne.Targets[0].Backend != "alpha" {
		t.Errorf("chose %q, want the first backend in preference order", repOne.Targets[0].Backend)
	}
}

// A name-addressed backend (Ollama) can only be trusted to run this unit
// when the unit's own source names it. Otherwise it would evaluate
// whatever model happens to share the name — a different unit entirely.
func TestNameAddressedBackendNeedsMatchingSource(t *testing.T) {
	newOllamaish := func() *stubBackend {
		return &stubBackend{
			name: "ollama",
			cap:  engine.Capability{Available: true, AcceptsModelPath: false, Devices: []string{"gpu", "cpu"}},
			gen: func(engine.Request, int) (*engine.Result, error) {
				return &engine.Result{Text: "ok", EvalTPS: 10}, nil
			},
		}
	}
	s := suiteOf(passCase("only", "ok"))

	matching := baseOpts(s, newOllamaish())
	matching.ModelRef = "ollama:phi3"
	rep, err := Run(context.Background(), matching)
	if err != nil {
		t.Fatalf("Run() with a matching source error = %v", err)
	}
	// It places work itself, so the device is reported as chosen by the
	// backend rather than claimed to be one NexusRun selected.
	if rep.Targets[0].Device != DeviceAuto {
		t.Errorf("device = %q, want %q", rep.Targets[0].Device, DeviceAuto)
	}
	if rep.Targets[0].Note == "" {
		t.Error("no note explaining that the backend chose the device")
	}

	mismatched := baseOpts(s, newOllamaish())
	mismatched.ModelRef = "hf:org/repo/model.gguf"
	if _, err := Run(context.Background(), mismatched); err == nil {
		t.Error("Run() used a name-addressed backend for a unit it cannot address")
	}

	// It cannot honour --device either, since it picks placement itself.
	pinned := baseOpts(s, newOllamaish())
	pinned.ModelRef = "ollama:phi3"
	pinned.Devices = []string{"cpu"}
	if _, err := Run(context.Background(), pinned); err == nil {
		t.Error("Run() pinned a device on a backend that selects its own")
	}
}

// Capability alone is not enough: the device has to exist on this host.
func TestUndetectedDeviceIsNotATarget(t *testing.T) {
	exotic := &stubBackend{
		name: "exotic",
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"quantum"}},
		gen: func(engine.Request, int) (*engine.Result, error) {
			return &engine.Result{Text: "ok"}, nil
		},
	}
	_, err := Run(context.Background(), baseOpts(suiteOf(passCase("only", "ok")), exotic))
	if err == nil {
		t.Fatal("Run() selected a device the host does not have")
	}
	if !strings.Contains(err.Error(), "no usable backend") {
		t.Errorf("error = %v, want it to explain there is no usable pair", err)
	}
}

func TestUnavailableBackendIsExplained(t *testing.T) {
	off := &stubBackend{
		name: "off",
		cap:  engine.Capability{Available: false, Detail: "llama-cli not found in PATH"},
		gen: func(engine.Request, int) (*engine.Result, error) {
			return nil, errors.New("unreachable")
		},
	}
	_, err := Run(context.Background(), baseOpts(suiteOf(passCase("only", "ok")), off))
	if err == nil {
		t.Fatal("Run() succeeded with no available backend")
	}
	// The reason each backend was rejected has to reach the user, or the
	// only advice possible is "it didn't work".
	if !strings.Contains(err.Error(), "llama-cli not found in PATH") {
		t.Errorf("error = %v, want it to carry the backend's own detail", err)
	}
}

// Precedence lets a suite evaluate a candidate prompt without editing the
// unit, and a case override a prompt for one awkward question.
func TestSystemPromptPrecedence(t *testing.T) {
	var seen []string
	spy := &stubBackend{
		name: "spy",
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}},
		gen: func(req engine.Request, _ int) (*engine.Result, error) {
			seen = append(seen, req.System)
			return &engine.Result{Text: "ok"}, nil
		},
	}
	s := suiteOf(
		passCase("inherits-suite", "ok"),
		Case{Name: "overrides", Prompt: "p", System: "from case", Expect: Expect{Contains: []string{"ok"}}},
	)
	s.System = "from suite"
	opts := baseOpts(s, spy)
	opts.System = "from unit"

	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if seen[0] != "from suite" {
		t.Errorf("case 1 system = %q, want the suite's", seen[0])
	}
	if seen[1] != "from case" {
		t.Errorf("case 2 system = %q, want the case's", seen[1])
	}

	// With no suite override, the unit's own prompt is what runs — the
	// same text `nexus run` would use.
	seen = nil
	bare := suiteOf(passCase("inherits-unit", "ok"))
	optsBare := baseOpts(bare, spy)
	optsBare.System = "from unit"
	if _, err := Run(context.Background(), optsBare); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if seen[0] != "from unit" {
		t.Errorf("system = %q, want the unit's", seen[0])
	}
}

// Evaluating without the chat template would measure a different thing
// than running the unit does, making the score meaningless.
func TestChatTemplateAndSamplingReachTheBackend(t *testing.T) {
	var got engine.Request
	spy := &stubBackend{
		name: "spy",
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}},
		gen: func(req engine.Request, _ int) (*engine.Result, error) {
			got = req
			return &engine.Result{Text: "ok"}, nil
		},
	}
	temp := 0.4
	s := suiteOf(Case{Name: "c", Prompt: "p", MaxTokens: 32, Expect: Expect{Contains: []string{"ok"}}})
	s.Temperature = &temp
	s.MaxTokens = 128
	opts := baseOpts(s, spy)
	opts.Chat = true
	opts.Context = 8192

	rep, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !got.Chat {
		t.Error("Chat was not passed through to the backend")
	}
	if got.Temperature != 0.4 {
		t.Errorf("Temperature = %v, want 0.4", got.Temperature)
	}
	if got.MaxTokens != 32 {
		t.Errorf("MaxTokens = %d, want the case override 32", got.MaxTokens)
	}
	if got.Context != 8192 {
		t.Errorf("Context = %d, want 8192", got.Context)
	}
	if rep.Temperature != 0.4 || rep.MaxTokens != 128 {
		t.Errorf("report recorded temperature %v / max %d, want 0.4 / 128", rep.Temperature, rep.MaxTokens)
	}
}

func TestQuantHint(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/models/Llama-3.2-1B-Instruct-Q4_K_M.gguf", "Q4_K_M"},
		{"/models/model.IQ3_XS.gguf", "IQ3_XS"},
		{"/models/model-f16.gguf", "F16"},
		{"/models/model_bf16.gguf", "BF16"},
		// Weights borrowed from Ollama are stored under their digest, so
		// there is no filename to read. The digest still pins them.
		{"/home/u/.ollama/models/blobs/sha256-1a2b3c", ""},
		{"/models/plain.gguf", ""},
	}
	for _, tt := range tests {
		if got := quantHint(tt.in); got != tt.want {
			t.Errorf("quantHint(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReportStringShowsFailuresAndProvenance(t *testing.T) {
	s := suiteOf(passCase("finds-code", "E_TIMEOUT"), passCase("missing", "NOPE"))
	rep, err := Run(context.Background(), baseOpts(s, cpuStub("stub", "the code is E_TIMEOUT")))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	out := rep.String()
	for _, want := range []string{"test-suite", "test-unit:0.1.0", "Q4_K_M", "1/2", "50.0%", "missing", "NOPE"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q:\n%s", want, out)
		}
	}
}

// The header must still name the real case count when the first target
// turned out to be unusable — that is exactly when it is read.
func TestReportHeaderSurvivesAnUnusableFirstTarget(t *testing.T) {
	broken := &stubBackend{
		name: "broken",
		cap:  engine.Capability{Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}},
		gen: func(engine.Request, int) (*engine.Result, error) {
			return nil, errors.New("no CUDA backend compiled in")
		},
	}
	opts := baseOpts(suiteOf(passCase("a", "ok"), passCase("b", "ok")), broken, cpuStub("working", "ok"))
	opts.AllDevices = true

	rep, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := rep.caseCount(); got != 2 {
		t.Errorf("caseCount() = %d, want 2", got)
	}
	if !strings.Contains(rep.String(), "2 cases") {
		t.Errorf("header does not report 2 cases:\n%s", rep.String())
	}
	// The working target still has to be evaluated and reported.
	if best := rep.Best(); best == nil || best.Backend != "working" {
		t.Errorf("Best() = %+v, want the working backend", best)
	}
}
