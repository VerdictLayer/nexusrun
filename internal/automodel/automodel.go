// Package automodel picks the model a unit should run with on this
// specific machine, by measuring candidates rather than trusting a
// hardcoded choice.
//
// The problem it solves is the one that shows up when the same agent is
// shipped to fifty devices: llama3.1:8b is the obvious pick on a
// workstation and unusable on a Pi, phi3 is the reverse, and nobody
// authoring the unit knows which machine it will land on. The unit
// declares the bar instead — context window, size ceiling, how many eval
// cases must pass — and each machine measures which candidate clears it.
//
// Measuring is expensive, so results are cached per machine fingerprint
// and reused until they go stale. The first run on a new device pays for
// the evaluation; every run after it is a cache read.
package automodel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/bench"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/eval"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/store"
)

// Options configures a selection.
type Options struct {
	Store    *store.Store
	Manifest *manifest.Manifest

	// Model is the auto-selection entry being resolved.
	Model manifest.Model

	// Suite is the unit's eval suite. It is what "quality" means here; a
	// profile with a min_quality_score and no suite cannot be resolved.
	Suite     *eval.Suite
	SuitePath string

	UnitDigest string

	Backends []engine.Backend
	Prefer   []string

	// CacheTTL overrides how long a cached measurement is trusted.
	CacheTTL time.Duration

	// Refresh forces re-measurement, ignoring cached results.
	Refresh bool

	// DryRun measures and reports without writing the cache — what
	// `nexus bench <unit>` does.
	DryRun bool

	Progress func(format string, args ...any)
}

// Considered is one candidate's fate, kept so the decision can be
// explained. A selection the user cannot audit is a selection they will
// override with a hardcoded model the first time it surprises them.
type Considered struct {
	Source    string  `json:"source"`
	Selected  bool    `json:"selected"`
	Rejected  string  `json:"rejected,omitempty"` // why it was excluded
	Cached    bool    `json:"cached,omitempty"`
	Passed    int     `json:"passed,omitempty"`
	Total     int     `json:"total,omitempty"`
	TokPerSec float64 `json:"tok_per_sec,omitempty"`
	SizeBytes int64   `json:"size_bytes,omitempty"`
	Weight    float64 `json:"weight,omitempty"`
	Score     float64 `json:"score,omitempty"`
	Backend   string  `json:"backend,omitempty"`
	Device    string  `json:"device,omitempty"`
}

// Decision is the outcome of a selection.
type Decision struct {
	Profile    string       `json:"profile"`
	Source     string       `json:"source"`
	Path       string       `json:"path"`
	Digest     string       `json:"digest,omitempty"`
	Context    int          `json:"context,omitempty"`
	Strategy   string       `json:"strategy"`
	Reason     string       `json:"reason"`
	Considered []Considered `json:"considered"`
}

// Select resolves an auto-model profile to one concrete model.
func Select(ctx context.Context, opts Options) (*Decision, error) {
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mod := opts.Model
	if !mod.Auto() {
		return nil, fmt.Errorf("model %q declares no candidates; there is nothing to select between", mod.ID)
	}
	req := mod.Requirements
	if req == nil {
		req = &manifest.Requirements{}
	}
	strategy := mod.Strategy()
	profile := mod.ProfileName()

	if req.MinQualityScore > 0 && opts.Suite == nil {
		return nil, fmt.Errorf(
			"model %q requires min_quality_score %d but the unit has no eval suite — quality is measured by running the suite, so add one under evals/",
			mod.ID, req.MinQualityScore)
	}

	backends := opts.Backends
	if req.ToolCalling == manifest.ToolCallingRequired {
		// A required tool-calling model measured on a backend that cannot
		// carry tool calls would score well and then fail in production.
		backends = toolCapable(backends)
		if len(backends) == 0 {
			return nil, fmt.Errorf(
				"model %q requires tool calling, but no installed backend can carry tool calls — run `nexus doctor`",
				mod.ID)
		}
	}

	cache, err := bench.LoadCache(opts.Store)
	if err != nil {
		return nil, err
	}
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = bench.DefaultTTL
	}

	dec := &Decision{Profile: profile, Strategy: strategy}
	type scored struct {
		cand manifest.Candidate
		res  *store.ResolvedModel
		idx  int
	}
	var viable []scored

	// Step 1: hard requirements. These are checked before any generation,
	// because rejecting a candidate on its size is free and evaluating it
	// is minutes.
	for _, c := range opts.Model.Candidates {
		row := Considered{Source: c.Source, Weight: weightOf(c)}

		window := c.Context
		if window == 0 {
			window = mod.Context
		}
		if req.MinContext > 0 && window > 0 && window < req.MinContext {
			row.Rejected = fmt.Sprintf("context %d < required %d", window, req.MinContext)
			dec.Considered = append(dec.Considered, row)
			logf("skip %s — %s", c.Source, row.Rejected)
			continue
		}

		resolved, rerr := opts.Store.Resolve(c.Source, c.SHA256, nil)
		if rerr != nil {
			// A candidate that cannot be fetched is not a failure of the
			// unit: the point of a candidate list is that some of them are
			// absent on any given machine.
			row.Rejected = "unavailable: " + firstLine(rerr.Error())
			dec.Considered = append(dec.Considered, row)
			logf("skip %s — %s", c.Source, row.Rejected)
			continue
		}
		row.SizeBytes = resolved.Size
		if req.MaxSizeMB > 0 && resolved.Size > req.MaxSizeMB*1024*1024 {
			row.Rejected = fmt.Sprintf("%.0f MB > max_size_mb %d",
				float64(resolved.Size)/(1024*1024), req.MaxSizeMB)
			dec.Considered = append(dec.Considered, row)
			logf("skip %s — %s", c.Source, row.Rejected)
			continue
		}

		dec.Considered = append(dec.Considered, row)
		viable = append(viable, scored{cand: c, res: resolved, idx: len(dec.Considered) - 1})
	}

	if len(viable) == 0 {
		return nil, fmt.Errorf("no candidate for model %q met the requirements:\n%s",
			mod.ID, indentRejections(dec.Considered))
	}

	// Step 2: measure, preferring a fresh cached result.
	for _, v := range viable {
		row := &dec.Considered[v.idx]
		key := bench.Key{
			Unit: opts.Manifest.Name, Version: opts.Manifest.Version,
			Profile: profile, Model: v.cand.Source,
		}

		if !opts.Refresh {
			if e := cache.Lookup(key, opts.UnitDigest, ttl); e != nil {
				row.Cached = true
				row.Passed, row.Total = e.Passed, e.Total
				row.TokPerSec = e.TokPerSec
				row.Backend, row.Device = e.Backend, e.Device
				logf("%s — cached %s at %.1f tok/s (%s)", v.cand.Source, e.EvalScore, e.TokPerSec, e.Backend+"/"+e.Device)
				continue
			}
		}

		if opts.Suite == nil {
			// With no suite there is nothing to score, so throughput alone
			// decides. That is a legitimate profile — "any of these work,
			// give me the fastest" — and is why a suite is not mandatory.
			logf("measuring %s (no suite; throughput only)…", v.cand.Source)
			tps, backend, device, berr := measureThroughput(ctx, backends, v.res.Path, opts.Prefer)
			if berr != nil {
				row.Rejected = "could not run: " + firstLine(berr.Error())
				logf("skip %s — %s", v.cand.Source, row.Rejected)
				continue
			}
			row.TokPerSec, row.Backend, row.Device = tps, backend, device
			cache.Put(bench.Entry{
				Unit: key.Unit, Version: key.Version, Profile: profile, Model: v.cand.Source,
				Backend: backend, Device: device, TokPerSec: tps,
				ModelSizeBytes: v.res.Size, ModelDigest: v.res.Digest,
				UnitDigest: opts.UnitDigest, Timestamp: time.Now().UTC(),
			})
			continue
		}

		logf("evaluating %s against %d case(s)…", v.cand.Source, len(opts.Suite.Cases))
		window := v.cand.Context
		if window == 0 {
			window = mod.Context
		}
		rep, rerr := eval.Run(ctx, eval.Options{
			Suite:         opts.Suite,
			SuitePath:     opts.SuitePath,
			UnitRef:       opts.Manifest.Ref(),
			UnitDigest:    opts.UnitDigest,
			ModelPath:     v.res.Path,
			ModelRef:      v.cand.Source,
			ModelDigest:   v.res.Digest,
			ModelQuant:    v.res.Quant,
			ModelParams:   v.res.Params,
			ModelSize:     v.res.Size,
			ModelOverride: true,
			System:        opts.Manifest.Entrypoint.SystemPrompt,
			Context:       window,
			Chat:          true,
			Repeats:       1,
			Prefer:        opts.Prefer,
			Backends:      backends,
			Progress:      func(string, ...any) {}, // the sweep prints its own summary
		})
		if rerr != nil {
			row.Rejected = "could not run: " + firstLine(rerr.Error())
			logf("skip %s — %s", v.cand.Source, row.Rejected)
			continue
		}
		best := rep.Best()
		if best == nil {
			row.Rejected = "no backend could execute the suite"
			logf("skip %s — %s", v.cand.Source, row.Rejected)
			continue
		}
		row.Passed, row.Total = best.Passed, best.Total
		row.TokPerSec = best.EvalTPS
		row.Backend, row.Device = best.Backend, best.Device
		logf("%s — %d/%d at %.1f tok/s (%s)", v.cand.Source, best.Passed, best.Total, best.EvalTPS, best.Label())

		cache.Put(bench.Entry{
			Unit: key.Unit, Version: key.Version, Profile: profile, Model: v.cand.Source,
			Backend: best.Backend, Device: best.Device,
			Passed: best.Passed, Total: best.Total,
			TokPerSec:      best.EvalTPS,
			ModelSizeBytes: v.res.Size, ModelDigest: v.res.Digest,
			UnitDigest: opts.UnitDigest, Timestamp: time.Now().UTC(),
		})
	}

	if !opts.DryRun {
		if err := bench.SaveCache(opts.Store, cache); err != nil {
			logf("warning: could not write benchmark cache: %v", err)
		}
	}

	// Step 3: apply the strategy.
	pickIdx, reason, err := pick(dec.Considered, req, strategy)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", mod.ID, err)
	}
	dec.Considered[pickIdx].Selected = true
	winner := dec.Considered[pickIdx]
	dec.Source, dec.Reason = winner.Source, reason

	for _, v := range viable {
		if v.cand.Source == winner.Source {
			dec.Path, dec.Digest = v.res.Path, v.res.Digest
			dec.Context = v.cand.Context
			if dec.Context == 0 {
				dec.Context = mod.Context
			}
		}
	}
	return dec, nil
}

// pick applies the selection strategy to the measured candidates.
func pick(rows []Considered, req *manifest.Requirements, strategy string) (int, string, error) {
	type entry struct {
		idx int
		row Considered
	}
	var eligible []entry
	for i, r := range rows {
		if r.Rejected != "" {
			continue
		}
		if req.MinQualityScore > 0 && r.Passed < req.MinQualityScore {
			rows[i].Rejected = fmt.Sprintf("scored %d/%d, below min_quality_score %d",
				r.Passed, r.Total, req.MinQualityScore)
			continue
		}
		eligible = append(eligible, entry{idx: i, row: r})
	}
	if len(eligible) == 0 {
		return 0, "", fmt.Errorf("no candidate cleared the requirements:\n%s", indentRejections(rows))
	}

	switch strategy {
	case manifest.StrategyFastestPassing:
		sort.SliceStable(eligible, func(a, b int) bool {
			return eligible[a].row.TokPerSec > eligible[b].row.TokPerSec
		})
		w := eligible[0].row
		return eligible[0].idx, fmt.Sprintf("fastest of %d candidate(s) meeting the bar (%.1f tok/s, %d/%d)",
			len(eligible), w.TokPerSec, w.Passed, w.Total), nil

	case manifest.StrategySmallestPassing:
		sort.SliceStable(eligible, func(a, b int) bool {
			return eligible[a].row.SizeBytes < eligible[b].row.SizeBytes
		})
		w := eligible[0].row
		return eligible[0].idx, fmt.Sprintf("smallest of %d candidate(s) meeting the bar (%.0f MB, %d/%d)",
			len(eligible), float64(w.SizeBytes)/(1024*1024), w.Passed, w.Total), nil

	default: // best_eval_score
		// The author's weight breaks ties between candidates that scored
		// the same, and throughput breaks what the weight does not. A
		// weight cannot promote a candidate over one that genuinely scored
		// better — it is a preference, not a thumb on the scale.
		for i := range eligible {
			eligible[i].row.Score = eligible[i].row.rate() * weightOrOne(eligible[i].row.Weight)
			rows[eligible[i].idx].Score = eligible[i].row.Score
		}
		sort.SliceStable(eligible, func(a, b int) bool {
			ra, rb := eligible[a].row, eligible[b].row
			if ra.rate() != rb.rate() {
				return ra.rate() > rb.rate()
			}
			if ra.Score != rb.Score {
				return ra.Score > rb.Score
			}
			return ra.TokPerSec > rb.TokPerSec
		})
		w := eligible[0].row
		if w.Total == 0 {
			return eligible[0].idx, fmt.Sprintf("fastest of %d candidate(s); no suite to score against (%.1f tok/s)",
				len(eligible), w.TokPerSec), nil
		}
		return eligible[0].idx, fmt.Sprintf("best score of %d candidate(s): %d/%d at %.1f tok/s",
			len(eligible), w.Passed, w.Total, w.TokPerSec), nil
	}
}

func (c Considered) rate() float64 {
	if c.Total == 0 {
		return 0
	}
	return 100 * float64(c.Passed) / float64(c.Total)
}

// String renders a decision as the table `nexus run --auto-model` prints.
func (d *Decision) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Auto-model (profile %q, strategy %s)\n\n", d.Profile, d.Strategy)

	w := len("MODEL")
	for _, c := range d.Considered {
		w = max(w, len(c.Source))
	}
	row := fmt.Sprintf("  %%-1s %%-%ds  %%9s  %%9s  %%8s  %%s\n", w)
	fmt.Fprintf(&b, row, "", "MODEL", "SCORE", "TOK/S", "SIZE", "NOTE")
	for _, c := range d.Considered {
		mark := " "
		if c.Selected {
			mark = "→"
		}
		score, tps, size, note := "—", "—", "—", ""
		if c.Total > 0 {
			score = fmt.Sprintf("%d/%d", c.Passed, c.Total)
		}
		if c.TokPerSec > 0 {
			tps = fmt.Sprintf("%.1f", c.TokPerSec)
		}
		if c.SizeBytes > 0 {
			size = fmt.Sprintf("%.0f MB", float64(c.SizeBytes)/(1024*1024))
		}
		switch {
		case c.Rejected != "":
			note = "rejected: " + c.Rejected
		case c.Cached:
			note = "cached"
		}
		fmt.Fprintf(&b, row, mark, c.Source, score, tps, size, note)
	}
	fmt.Fprintf(&b, "\nSelected %s — %s\n", d.Source, d.Reason)
	return b.String()
}

// measureThroughput generates once on the preferred target and reports
// tokens/sec, for profiles with no suite to score.
func measureThroughput(ctx context.Context, backends []engine.Backend, modelPath string, prefer []string) (float64, string, string, error) {
	b, device, err := engine.SelectFrom(backends, hardware.Detect(), prefer)
	if err != nil {
		return 0, "", "", err
	}
	res, err := b.Generate(ctx, engine.Request{
		ModelPath:   modelPath,
		Prompt:      "Explain what a neural processing unit does, in two sentences.",
		MaxTokens:   64,
		Temperature: 0,
		Device:      device,
		Chat:        true,
	})
	if err != nil {
		return 0, "", "", err
	}
	return res.EvalTPS, b.Name(), device, nil
}

func toolCapable(backends []engine.Backend) []engine.Backend {
	var out []engine.Backend
	for _, b := range backends {
		if cap := b.Probe(); cap.Available && cap.SupportsTools {
			out = append(out, b)
		}
	}
	return out
}

func weightOf(c manifest.Candidate) float64 { return weightOrOne(c.Weight) }

func weightOrOne(w float64) float64 {
	if w <= 0 {
		return 1
	}
	return w
}

func indentRejections(rows []Considered) string {
	var b strings.Builder
	for _, r := range rows {
		if r.Rejected == "" {
			continue
		}
		fmt.Fprintf(&b, "  %s — %s\n", r.Source, r.Rejected)
	}
	if b.Len() == 0 {
		return "  (no candidates declared)\n"
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
