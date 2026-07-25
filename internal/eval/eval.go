package eval

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/hardware"
)

// DeviceAuto labels a target whose backend places work itself. Ollama is
// the case that matters: it reports both GPU and CPU capability but
// chooses between them internally, so claiming the suite ran "on GPU"
// would be an assertion NexusRun cannot make.
const DeviceAuto = "auto"

// Options configures an evaluation run.
type Options struct {
	Suite *Suite

	// SuitePath is recorded in the report for provenance.
	SuitePath string

	// Unit identity. UnitDigest is what makes a score citable: the same
	// suite against a different digest is a different measurement.
	UnitRef    string
	UnitDigest string

	// Model identity. ModelPath is what path-addressed backends execute;
	// ModelRef is the manifest source, which name-addressed backends need.
	ModelPath   string
	ModelRef    string
	ModelDigest string

	// System is the unit's system prompt, used unless the suite or case
	// overrides it.
	System string

	// Context is the unit's context window.
	Context int

	// Chat applies the model's chat template, as `nexus run` does for
	// chat units. Evaluating without it measures a different thing than
	// running the unit does, which would make the score a lie.
	Chat bool

	// Repeats runs every case this many times. Above 1, a case that does
	// not pass every repeat is reported as flaky rather than passing,
	// which is how sampling instability becomes visible instead of
	// intermittent.
	Repeats int

	// Devices restricts evaluation to these accelerator classes.
	Devices []string

	// AllDevices evaluates every usable backend/device pair instead of
	// only the one the unit would normally select. This is what answers
	// "does this unit still work on CPU?" before shipping it to a fleet
	// where most hosts have nothing else.
	AllDevices bool

	// Prefer is the unit's hardware preference order, used to pick the
	// single target when AllDevices is false.
	Prefer []string

	// Backends overrides the backend set. Injecting it is what makes
	// accelerator paths testable on a machine that has none, the same
	// reason engine.SelectFrom takes its candidates as a parameter.
	Backends []engine.Backend

	Progress func(format string, args ...any)
}

// CaseResult aggregates one case's repeats on one target.
type CaseResult struct {
	Name    string  `json:"name"`
	Runs    int     `json:"runs"`
	Passes  int     `json:"passes"`
	Flaky   bool    `json:"flaky,omitempty"`
	EvalTPS float64 `json:"eval_tokens_per_sec,omitempty"`

	// Failures holds the failed assertions from the first repeat that
	// failed, which is the one worth showing.
	Failures []Check `json:"failures,omitempty"`

	// Output is a truncated sample, kept so a diff between two runs can
	// show what changed and not merely that something did.
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Passed reports whether the case passed every repeat. Anything less is
// not a pass: a case that succeeds two times in three tells you the unit
// is unreliable, and averaging that away is how flaky agents reach
// production.
func (c CaseResult) Passed() bool { return c.Runs > 0 && c.Passes == c.Runs && c.Error == "" }

// Status is a short label for reporting and diffing.
func (c CaseResult) Status() string {
	switch {
	case c.Error != "":
		return "error"
	case c.Passed():
		return "pass"
	case c.Passes > 0:
		return "flaky"
	default:
		return "fail"
	}
}

// TargetResult is the whole suite's outcome on one backend/device pair.
type TargetResult struct {
	Backend string       `json:"backend"`
	Device  string       `json:"device"`
	Note    string       `json:"note,omitempty"`
	Cases   []CaseResult `json:"cases"`
	Passed  int          `json:"passed"`
	Total   int          `json:"total"`
	Flaky   int          `json:"flaky"`
	EvalTPS float64      `json:"eval_tokens_per_sec,omitempty"` // median across cases

	// Error is set when the backend could not run the suite at all, which
	// is reported separately from cases failing. A backend that cannot
	// execute has produced no evidence about the unit's quality.
	Error string `json:"error,omitempty"`
}

// Rate is the pass rate as a percentage.
func (t TargetResult) Rate() float64 {
	if t.Total == 0 {
		return 0
	}
	return 100 * float64(t.Passed) / float64(t.Total)
}

// Label is the target's stable identifier, used as a diff key.
func (t TargetResult) Label() string { return t.Backend + "/" + t.Device }

// Report is one evaluation of one unit under one set of conditions.
type Report struct {
	ID          string           `json:"id"`
	Suite       string           `json:"suite"`
	SuitePath   string           `json:"suite_path,omitempty"`
	Unit        string           `json:"unit,omitempty"`
	UnitDigest  string           `json:"unit_digest,omitempty"`
	Model       string           `json:"model,omitempty"`
	ModelDigest string           `json:"model_digest,omitempty"`
	ModelQuant  string           `json:"model_quant,omitempty"`
	Host        *hardware.Report `json:"host"`
	Repeats     int              `json:"repeats"`
	Temperature float64          `json:"temperature"`
	MaxTokens   int              `json:"max_tokens"`
	Targets     []TargetResult   `json:"targets"`
	Timestamp   time.Time        `json:"timestamp"`
	Took        time.Duration    `json:"took"`
}

// Best returns the highest-scoring target, or nil when none ran.
func (r *Report) Best() *TargetResult {
	var best *TargetResult
	for i := range r.Targets {
		t := &r.Targets[i]
		if t.Error != "" {
			continue
		}
		if best == nil || t.Rate() > best.Rate() {
			best = t
		}
	}
	return best
}

// Run evaluates the suite on every selected target.
func Run(ctx context.Context, opts Options) (*Report, error) {
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.Suite == nil {
		return nil, errors.New("no eval suite given")
	}
	if err := opts.Suite.Validate(); err != nil {
		return nil, err
	}
	if opts.Repeats <= 0 {
		opts.Repeats = 1
	}
	temp := 0.0
	if opts.Suite.Temperature != nil {
		temp = *opts.Suite.Temperature
	}
	maxTok := opts.Suite.MaxTokens
	if maxTok <= 0 {
		maxTok = 256
	}

	hw := hardware.Detect()
	cands, err := selectCandidates(opts, hw, logf)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	rep := &Report{
		ID:          time.Now().UTC().Format("20060102T150405Z") + "-" + slug(opts.Suite.Name),
		Suite:       opts.Suite.Name,
		SuitePath:   opts.SuitePath,
		Unit:        opts.UnitRef,
		UnitDigest:  opts.UnitDigest,
		Model:       opts.ModelRef,
		ModelDigest: opts.ModelDigest,
		ModelQuant:  quantHint(opts.ModelPath),
		Host:        hw,
		Repeats:     opts.Repeats,
		Temperature: temp,
		MaxTokens:   maxTok,
		Timestamp:   time.Now(),
	}

	for _, c := range cands {
		logf("evaluating %d cases on %s/%s (%d repeat(s))…",
			len(opts.Suite.Cases), c.backend.Name(), strings.ToUpper(c.device), opts.Repeats)
		tr := runTarget(ctx, c, opts, temp, maxTok, logf)
		rep.Targets = append(rep.Targets, tr)
		if tr.Error != "" {
			logf("  %s/%s unusable: %s", c.backend.Name(), c.device, firstLine(tr.Error))
			continue
		}
		logf("  %d/%d passed (%.1f%%)", tr.Passed, tr.Total, tr.Rate())
	}
	rep.Took = time.Since(start)
	return rep, nil
}

// candidate is one backend/device pair to evaluate on.
type candidate struct {
	backend engine.Backend
	device  string // reported label
	reqDev  string // what the backend is asked for; empty when it chooses
	note    string
}

// selectCandidates builds the target list from probed backend capability
// intersected with detected hardware — never from hardware presence, for
// the same reason the scheduler doesn't: a device nothing can drive would
// produce a score for an execution path that does not exist.
func selectCandidates(opts Options, hw *hardware.Report, logf func(string, ...any)) ([]candidate, error) {
	backends := opts.Backends
	if backends == nil {
		backends = engine.All()
	}

	var cands []candidate
	var why []string
	seen := map[string]bool{}
	for _, b := range backends {
		cap := b.Probe()
		if !cap.Available {
			why = append(why, fmt.Sprintf("%s: %s", b.Name(), cap.Detail))
			continue
		}
		if !cap.AcceptsModelPath {
			// A name-addressed backend can only run this unit when the
			// unit's own model source names that backend — "ollama:phi3"
			// for the ollama backend. Otherwise it would evaluate whatever
			// model happens to share the name, which is a different unit.
			if !strings.HasPrefix(opts.ModelRef, cap.Backend+":") {
				why = append(why, fmt.Sprintf("%s: addresses models by name, and this unit's source is %q", b.Name(), opts.ModelRef))
				continue
			}
			if len(opts.Devices) > 0 {
				// It picks its own device, so it cannot honour --device.
				// Silently including it would attribute its score to a
				// device the caller only thinks was used.
				why = append(why, fmt.Sprintf("%s: chooses its own device, cannot be pinned with --device", b.Name()))
				continue
			}
			cands = append(cands, candidate{
				backend: b, device: DeviceAuto,
				note: "backend selects its own device",
			})
			continue
		}
		for _, d := range cap.Devices {
			if !hw.Has(d) {
				logf("skip %s/%s — backend supports it but no such device detected", b.Name(), d)
				continue
			}
			if len(opts.Devices) > 0 && !containsStr(opts.Devices, d) {
				continue
			}
			key := b.Name() + "/" + d
			if seen[key] {
				continue
			}
			seen[key] = true
			cands = append(cands, candidate{backend: b, device: d, reqDev: d})
		}
	}
	if len(cands) == 0 {
		msg := "no usable backend/device pair to evaluate on"
		if len(why) > 0 {
			msg += ":\n  " + strings.Join(why, "\n  ")
		}
		return nil, errors.New(msg + "\nrun `nexus doctor` for details")
	}
	if opts.AllDevices {
		return cands, nil
	}

	// Single-target mode: evaluate where the unit would actually run.
	for _, class := range opts.Prefer {
		for _, c := range cands {
			if c.device == class {
				return []candidate{c}, nil
			}
		}
	}
	return cands[:1], nil
}

// runTarget executes the whole suite on one target.
func runTarget(ctx context.Context, c candidate, opts Options, temp float64, maxTok int, logf func(string, ...any)) TargetResult {
	tr := TargetResult{Backend: c.backend.Name(), Device: c.device, Note: c.note}
	var tps []float64

	for i, cs := range opts.Suite.Cases {
		cr := CaseResult{Name: cs.Name}
		system := opts.System
		if opts.Suite.System != "" {
			system = opts.Suite.System
		}
		if cs.System != "" {
			system = cs.System
		}
		caseMax := maxTok
		if cs.MaxTokens > 0 {
			caseMax = cs.MaxTokens
		}

		var caseTPS []float64
		for run := 0; run < opts.Repeats; run++ {
			res, err := c.backend.Generate(ctx, engine.Request{
				ModelPath:   opts.ModelPath,
				ModelRef:    opts.ModelRef,
				Prompt:      cs.Prompt,
				System:      system,
				MaxTokens:   caseMax,
				Temperature: temp,
				Context:     opts.Context,
				Device:      c.reqDev,
				Chat:        opts.Chat,
			})
			if err != nil {
				if ctx.Err() != nil {
					cr.Error = ctx.Err().Error()
					break
				}
				// A failure on the very first generation means the backend
				// cannot execute this model at all — reported as a target
				// error rather than as every case failing, which would
				// look like the unit's fault.
				if i == 0 && run == 0 {
					tr.Error = err.Error()
					return tr
				}
				cr.Error = err.Error()
				break
			}
			cr.Runs++
			caseTPS = append(caseTPS, res.EvalTPS)
			checks := cs.Expect.Check(res.Text, opts.Suite.CaseSensitive)
			if Passed(checks) {
				cr.Passes++
			} else if cr.Failures == nil {
				cr.Failures = FailedChecks(checks)
				cr.Output = truncate(res.Text, 400)
			}
			if cr.Output == "" {
				cr.Output = truncate(res.Text, 400)
			}
		}
		cr.Flaky = cr.Passes > 0 && cr.Passes < cr.Runs
		cr.EvalTPS = median(caseTPS)
		if cr.EvalTPS > 0 {
			tps = append(tps, cr.EvalTPS)
		}
		tr.Cases = append(tr.Cases, cr)
		tr.Total++
		if cr.Passed() {
			tr.Passed++
		}
		if cr.Flaky {
			tr.Flaky++
		}
		logf("  [%d/%d] %-28s %s", i+1, len(opts.Suite.Cases), cs.Name, cr.Status())
	}
	tr.EvalTPS = median(tps)
	return tr
}

// String renders the report as a scorecard.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Suite:    %s (%d cases × %d repeat(s))\n", r.Suite, r.caseCount(), r.Repeats)
	if r.Unit != "" {
		fmt.Fprintf(&b, "Unit:     %s", r.Unit)
		if r.UnitDigest != "" {
			fmt.Fprintf(&b, " (%s)", shortDigest(r.UnitDigest))
		}
		b.WriteString("\n")
	}
	if r.Model != "" {
		fmt.Fprintf(&b, "Model:    %s", r.Model)
		quant := r.ModelQuant
		if quant == "" {
			quant = "quant unknown"
		}
		fmt.Fprintf(&b, " · %s", quant)
		if r.ModelDigest != "" {
			fmt.Fprintf(&b, " · %s", shortDigest(r.ModelDigest))
		}
		b.WriteString("\n")
	}
	if r.Host != nil {
		fmt.Fprintf(&b, "Host:     %s/%s · %s\n", r.Host.OS, r.Host.Arch, r.Host.CPUModel)
	}
	fmt.Fprintf(&b, "Sampling: temperature %.2f · max %d tokens\n\n", r.Temperature, r.MaxTokens)

	fmt.Fprintf(&b, "  %-8s %-16s %8s %8s %7s %11s\n", "DEVICE", "BACKEND", "PASS", "RATE", "FLAKY", "tok/s")
	fmt.Fprintf(&b, "  %s\n", strings.Repeat("─", 63))
	for _, t := range r.Targets {
		if t.Error != "" {
			fmt.Fprintf(&b, "  %-8s %-16s %s\n", strings.ToUpper(t.Device), t.Backend, "unusable: "+firstLine(t.Error))
			continue
		}
		fmt.Fprintf(&b, "  %-8s %-16s %8s %7.1f%% %7d %11.2f\n",
			strings.ToUpper(t.Device), t.Backend,
			fmt.Sprintf("%d/%d", t.Passed, t.Total), t.Rate(), t.Flaky, t.EvalTPS)
	}

	// Failures, per target, with the assertion that broke and what the
	// model actually said — the two things needed to fix a case.
	for _, t := range r.Targets {
		if t.Error != "" {
			continue
		}
		var bad []CaseResult
		for _, c := range t.Cases {
			if !c.Passed() {
				bad = append(bad, c)
			}
		}
		if len(bad) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s — %d not passing:\n", t.Label(), len(bad))
		for _, c := range bad {
			fmt.Fprintf(&b, "  %s  %s", c.Status(), c.Name)
			if c.Flaky {
				fmt.Fprintf(&b, " (%d/%d repeats passed)", c.Passes, c.Runs)
			}
			b.WriteString("\n")
			if c.Error != "" {
				fmt.Fprintf(&b, "      error: %s\n", firstLine(c.Error))
			}
			for _, f := range c.Failures {
				fmt.Fprintf(&b, "      expected %s %q", f.Kind, f.Want)
				if f.Detail != "" {
					fmt.Fprintf(&b, " — %s", f.Detail)
				}
				b.WriteString("\n")
			}
			if c.Output != "" {
				fmt.Fprintf(&b, "      got: %s\n", oneLine(truncate(c.Output, 160)))
			}
		}
	}

	if best := r.Best(); best != nil && len(r.Targets) > 1 {
		fmt.Fprintf(&b, "\nBest: %s at %.1f%%\n", best.Label(), best.Rate())
	}
	return b.String()
}

// caseCount is the widest case count any target reached. Taking the first
// target's would print "0 cases" whenever the first one turned out to be
// unusable, which is exactly when the header needs to be right.
func (r *Report) caseCount() int {
	n := 0
	for _, t := range r.Targets {
		if t.Total > n {
			n = t.Total
		}
	}
	return n
}

// --- helpers --------------------------------------------------------------

// reQuant matches the quantization tag conventionally embedded in GGUF
// filenames (Q4_K_M, IQ3_XS, f16). It is a hint, not an authority: models
// borrowed from Ollama are stored under their digest with no filename to
// read, in which case the digest itself identifies the weights exactly.
var reQuant = regexp.MustCompile(`(?i)(?:^|[-_.])(i?q\d+(?:_[a-z0-9]+)*|bf16|f16|f32)(?:[-_.]|$)`)

func quantHint(modelPath string) string {
	if m := reQuant.FindStringSubmatch(filepath.Base(modelPath)); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	return s[len(s)/2]
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "suite"
	}
	return out
}
