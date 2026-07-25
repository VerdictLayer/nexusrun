package eval

import (
	"fmt"
	"sort"
	"strings"
)

// CaseDelta is one case's status in two reports.
type CaseDelta struct {
	Name   string `json:"name"`
	Before string `json:"before"` // pass | fail | flaky | error | absent
	After  string `json:"after"`
}

// Changed reports whether the case's status moved.
func (c CaseDelta) Changed() bool { return c.Before != c.After }

// Regressed reports whether the case got worse. Anything leaving "pass"
// is a regression, including becoming flaky — a case that now passes only
// sometimes has stopped being evidence that the unit works.
func (c CaseDelta) Regressed() bool { return c.Before == "pass" && c.After != "pass" }

// Fixed reports whether the case started passing.
func (c CaseDelta) Fixed() bool { return c.Before != "pass" && c.After == "pass" }

// TargetDelta compares one backend/device pair across two reports.
type TargetDelta struct {
	Target     string      `json:"target"`
	BeforeRate float64     `json:"before_rate"`
	AfterRate  float64     `json:"after_rate"`
	Cases      []CaseDelta `json:"cases"`
	OnlyIn     string      `json:"only_in,omitempty"` // "before" or "after"
}

// RateDelta is the change in pass rate, in percentage points.
func (t TargetDelta) RateDelta() float64 { return t.AfterRate - t.BeforeRate }

// DiffReport is the comparison of two evaluations.
type DiffReport struct {
	BeforeID string        `json:"before_id"`
	AfterID  string        `json:"after_id"`
	Targets  []TargetDelta `json:"targets"`

	// Comparable is false when the two reports measured different things
	// — a different suite, unit, or model. The diff is still rendered,
	// with the mismatch stated, because silently comparing two unrelated
	// measurements is how misleading numbers get published.
	Comparable bool     `json:"comparable"`
	Mismatches []string `json:"mismatches,omitempty"`
}

// Regressions counts cases that stopped passing on any target.
func (d *DiffReport) Regressions() int {
	n := 0
	for _, t := range d.Targets {
		for _, c := range t.Cases {
			if c.Regressed() {
				n++
			}
		}
	}
	return n
}

// Diff compares two reports case by case, per target.
func Diff(before, after *Report) *DiffReport {
	d := &DiffReport{BeforeID: before.ID, AfterID: after.ID, Comparable: true}

	if before.Suite != after.Suite {
		d.Mismatches = append(d.Mismatches, fmt.Sprintf("different suites: %q vs %q", before.Suite, after.Suite))
	}
	if before.Unit != after.Unit {
		d.Mismatches = append(d.Mismatches, fmt.Sprintf("different units: %q vs %q", before.Unit, after.Unit))
	}
	// A rebuilt unit keeps its name:version while its contents change, so
	// the ref matching is not enough — without the digest check, comparing
	// two different builds of "agent:0.1.0" would look authoritative.
	if before.UnitDigest != "" && after.UnitDigest != "" && before.UnitDigest != after.UnitDigest {
		d.Mismatches = append(d.Mismatches, fmt.Sprintf("different unit builds: %s vs %s",
			shortDigest(before.UnitDigest), shortDigest(after.UnitDigest)))
	}
	if before.ModelDigest != "" && after.ModelDigest != "" && before.ModelDigest != after.ModelDigest {
		d.Mismatches = append(d.Mismatches, fmt.Sprintf("different model weights: %s vs %s",
			shortDigest(before.ModelDigest), shortDigest(after.ModelDigest)))
	}
	if before.Temperature != after.Temperature {
		d.Mismatches = append(d.Mismatches, fmt.Sprintf("different temperature: %.2f vs %.2f",
			before.Temperature, after.Temperature))
	}
	d.Comparable = len(d.Mismatches) == 0

	beforeTargets := indexTargets(before)
	afterTargets := indexTargets(after)

	var labels []string
	for l := range beforeTargets {
		labels = append(labels, l)
	}
	for l := range afterTargets {
		if _, ok := beforeTargets[l]; !ok {
			labels = append(labels, l)
		}
	}
	sort.Strings(labels)

	for _, label := range labels {
		bt, inBefore := beforeTargets[label]
		at, inAfter := afterTargets[label]
		td := TargetDelta{Target: label}
		switch {
		case !inAfter:
			td.OnlyIn = "before"
			td.BeforeRate = bt.Rate()
		case !inBefore:
			td.OnlyIn = "after"
			td.AfterRate = at.Rate()
		default:
			td.BeforeRate = bt.Rate()
			td.AfterRate = at.Rate()
		}

		statusOf := func(t *TargetResult) map[string]string {
			m := map[string]string{}
			if t == nil {
				return m
			}
			for _, c := range t.Cases {
				m[c.Name] = c.Status()
			}
			return m
		}
		bs, as := statusOf(bt), statusOf(at)

		var names []string
		for n := range bs {
			names = append(names, n)
		}
		for n := range as {
			if _, ok := bs[n]; !ok {
				names = append(names, n)
			}
		}
		sort.Strings(names)

		for _, n := range names {
			cd := CaseDelta{Name: n, Before: "absent", After: "absent"}
			if v, ok := bs[n]; ok {
				cd.Before = v
			}
			if v, ok := as[n]; ok {
				cd.After = v
			}
			td.Cases = append(td.Cases, cd)
		}
		d.Targets = append(d.Targets, td)
	}
	return d
}

func indexTargets(r *Report) map[string]*TargetResult {
	m := map[string]*TargetResult{}
	for i := range r.Targets {
		m[r.Targets[i].Label()] = &r.Targets[i]
	}
	return m
}

// String renders the diff, showing only what moved.
func (d *DiffReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Comparing %s → %s\n", d.BeforeID, d.AfterID)
	if !d.Comparable {
		b.WriteString("\nThese runs are not directly comparable:\n")
		for _, m := range d.Mismatches {
			fmt.Fprintf(&b, "  - %s\n", m)
		}
	}
	for _, t := range d.Targets {
		fmt.Fprintf(&b, "\n%s", t.Target)
		switch t.OnlyIn {
		case "before":
			fmt.Fprintf(&b, "  (only in %s — not evaluated in the newer run)\n", d.BeforeID)
		case "after":
			fmt.Fprintf(&b, "  (new target, %.1f%%)\n", t.AfterRate)
		default:
			fmt.Fprintf(&b, "  %.1f%% → %.1f%% (%+.1f pts)\n", t.BeforeRate, t.AfterRate, t.RateDelta())
		}
		changed := 0
		for _, c := range t.Cases {
			if !c.Changed() {
				continue
			}
			changed++
			marker := "~"
			switch {
			case c.Regressed():
				marker = "-"
			case c.Fixed():
				marker = "+"
			}
			fmt.Fprintf(&b, "  %s %-28s %s → %s\n", marker, c.Name, c.Before, c.After)
		}
		if changed == 0 && t.OnlyIn == "" {
			b.WriteString("  no case changed status\n")
		}
	}
	if n := d.Regressions(); n > 0 {
		fmt.Fprintf(&b, "\n%d case(s) stopped passing.\n", n)
	}
	return b.String()
}
