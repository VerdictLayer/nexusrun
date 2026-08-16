package automodel

import (
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/manifest"
)

func TestPickBestEvalScore(t *testing.T) {
	rows := []Considered{
		{Source: "small", Passed: 9, Total: 13, TokPerSec: 40},
		{Source: "big", Passed: 12, Total: 13, TokPerSec: 8},
		{Source: "mid", Passed: 11, Total: 13, TokPerSec: 20},
	}
	i, reason, err := pick(rows, &manifest.Requirements{}, manifest.StrategyBestEval)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "big" {
		t.Errorf("picked %q, want the highest scorer", rows[i].Source)
	}
	if !strings.Contains(reason, "12/13") {
		t.Errorf("reason should cite the score: %q", reason)
	}
}

func TestPickBreaksScoreTiesByWeightThenSpeed(t *testing.T) {
	rows := []Considered{
		{Source: "a", Passed: 12, Total: 13, TokPerSec: 10, Weight: 0.8},
		{Source: "b", Passed: 12, Total: 13, TokPerSec: 10, Weight: 1.0},
	}
	i, _, err := pick(rows, &manifest.Requirements{}, manifest.StrategyBestEval)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "b" {
		t.Errorf("picked %q, want the author-preferred candidate on a tie", rows[i].Source)
	}

	rows = []Considered{
		{Source: "slow", Passed: 12, Total: 13, TokPerSec: 5, Weight: 1},
		{Source: "fast", Passed: 12, Total: 13, TokPerSec: 25, Weight: 1},
	}
	i, _, err = pick(rows, &manifest.Requirements{}, manifest.StrategyBestEval)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "fast" {
		t.Errorf("picked %q, want the faster on an equal score and weight", rows[i].Source)
	}
}

func TestWeightCannotOverrideAGenuinelyBetterScore(t *testing.T) {
	// A weight is a preference between equals, not a thumb on the scale.
	rows := []Considered{
		{Source: "favoured-but-worse", Passed: 8, Total: 13, TokPerSec: 40, Weight: 1.0},
		{Source: "unfavoured-but-better", Passed: 12, Total: 13, TokPerSec: 8, Weight: 0.1},
	}
	i, _, err := pick(rows, &manifest.Requirements{}, manifest.StrategyBestEval)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "unfavoured-but-better" {
		t.Errorf("picked %q; a weight must not beat a real score difference", rows[i].Source)
	}
}

func TestPickFastestPassing(t *testing.T) {
	rows := []Considered{
		{Source: "best-quality", Passed: 13, Total: 13, TokPerSec: 4},
		{Source: "good-enough", Passed: 11, Total: 13, TokPerSec: 45},
		{Source: "too-weak", Passed: 6, Total: 13, TokPerSec: 90},
	}
	req := &manifest.Requirements{MinQualityScore: 11}
	i, reason, err := pick(rows, req, manifest.StrategyFastestPassing)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "good-enough" {
		t.Errorf("picked %q, want the fastest that clears the bar", rows[i].Source)
	}
	if !strings.Contains(reason, "fastest") {
		t.Errorf("reason = %q", reason)
	}
	// The candidate below the bar must be recorded as rejected, with why.
	for _, r := range rows {
		if r.Source == "too-weak" && !strings.Contains(r.Rejected, "min_quality_score") {
			t.Errorf("too-weak rejection = %q, should cite the bar", r.Rejected)
		}
	}
}

func TestPickSmallestPassing(t *testing.T) {
	rows := []Considered{
		{Source: "large", Passed: 13, Total: 13, SizeBytes: 8 << 30},
		{Source: "tiny", Passed: 11, Total: 13, SizeBytes: 900 << 20},
		{Source: "tiny-but-weak", Passed: 4, Total: 13, SizeBytes: 100 << 20},
	}
	req := &manifest.Requirements{MinQualityScore: 11}
	i, _, err := pick(rows, req, manifest.StrategySmallestPassing)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "tiny" {
		t.Errorf("picked %q, want the smallest that clears the bar", rows[i].Source)
	}
}

func TestPickIgnoresAlreadyRejected(t *testing.T) {
	rows := []Considered{
		{Source: "oversized", Rejected: "4000 MB > max_size_mb 3000", Passed: 13, Total: 13, TokPerSec: 50},
		{Source: "ok", Passed: 11, Total: 13, TokPerSec: 12},
	}
	i, _, err := pick(rows, &manifest.Requirements{}, manifest.StrategyBestEval)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "ok" {
		t.Errorf("picked %q; a candidate rejected on hard requirements must stay out", rows[i].Source)
	}
}

func TestPickFailsWhenNothingClearsTheBar(t *testing.T) {
	rows := []Considered{
		{Source: "a", Passed: 5, Total: 13},
		{Source: "b", Passed: 7, Total: 13},
	}
	_, _, err := pick(rows, &manifest.Requirements{MinQualityScore: 11}, manifest.StrategyBestEval)
	if err == nil {
		t.Fatal("expected an error when no candidate qualifies")
	}
	// The error must say what each candidate actually scored, or the user
	// has no way to decide whether to lower the bar or change the models.
	for _, want := range []string{"a", "b", "5/13", "7/13"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestPickWithNoSuiteRanksByThroughput(t *testing.T) {
	rows := []Considered{
		{Source: "slow", TokPerSec: 5},
		{Source: "fast", TokPerSec: 30},
	}
	i, reason, err := pick(rows, &manifest.Requirements{}, manifest.StrategyBestEval)
	if err != nil {
		t.Fatal(err)
	}
	if rows[i].Source != "fast" {
		t.Errorf("picked %q", rows[i].Source)
	}
	if !strings.Contains(reason, "no suite") {
		t.Errorf("reason should say no suite was scored: %q", reason)
	}
}

func TestDecisionStringShowsEveryCandidate(t *testing.T) {
	d := &Decision{
		Profile: "default", Strategy: manifest.StrategyBestEval,
		Source: "ollama:phi3:3.8b", Reason: "best score of 2 candidate(s): 12/13 at 14.0 tok/s",
		Considered: []Considered{
			{Source: "ollama:phi3:3.8b", Selected: true, Passed: 12, Total: 13, TokPerSec: 14, SizeBytes: 2 << 30},
			{Source: "ollama:llama3.1:8b", Rejected: "4600 MB > max_size_mb 3000", SizeBytes: 4600 << 20},
		},
	}
	out := d.String()
	for _, want := range []string{"ollama:phi3:3.8b", "12/13", "ollama:llama3.1:8b", "rejected", "max_size_mb", "→"} {
		if !strings.Contains(out, want) {
			t.Errorf("decision table should contain %q:\n%s", want, out)
		}
	}
}
