package eval

import (
	"strings"
	"testing"
)

func reportWith(id string, targets ...TargetResult) *Report {
	return &Report{ID: id, Suite: "s", Unit: "u:0.1.0", Targets: targets}
}

func target(label string, cases ...CaseResult) TargetResult {
	backend, device, _ := strings.Cut(label, "/")
	t := TargetResult{Backend: backend, Device: device, Cases: cases}
	for _, c := range cases {
		t.Total++
		if c.Passed() {
			t.Passed++
		}
		if c.Flaky {
			t.Flaky++
		}
	}
	return t
}

func passing(name string) CaseResult { return CaseResult{Name: name, Runs: 1, Passes: 1} }
func failing(name string) CaseResult { return CaseResult{Name: name, Runs: 1, Passes: 0} }
func flakyCase(name string) CaseResult {
	return CaseResult{Name: name, Runs: 2, Passes: 1, Flaky: true}
}

func TestDiffClassifiesMovement(t *testing.T) {
	before := reportWith("before", target("llama.cpp/cpu",
		passing("stays-passing"),
		passing("regresses"),
		failing("gets-fixed"),
		failing("stays-failing"),
		passing("becomes-flaky"),
	))
	after := reportWith("after", target("llama.cpp/cpu",
		passing("stays-passing"),
		failing("regresses"),
		passing("gets-fixed"),
		failing("stays-failing"),
		flakyCase("becomes-flaky"),
	))

	d := Diff(before, after)
	if len(d.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(d.Targets))
	}
	byName := map[string]CaseDelta{}
	for _, c := range d.Targets[0].Cases {
		byName[c.Name] = c
	}

	if c := byName["stays-passing"]; c.Changed() {
		t.Error("unchanged case reported as changed")
	}
	if c := byName["regresses"]; !c.Regressed() || c.Fixed() {
		t.Errorf("regression misclassified: %+v", c)
	}
	if c := byName["gets-fixed"]; !c.Fixed() || c.Regressed() {
		t.Errorf("fix misclassified: %+v", c)
	}
	if c := byName["stays-failing"]; c.Changed() {
		t.Error("still-failing case reported as changed")
	}
	// Becoming flaky is a regression. A case that now passes only
	// sometimes has stopped being evidence that the unit works.
	if c := byName["becomes-flaky"]; !c.Regressed() {
		t.Errorf("becoming flaky was not treated as a regression: %+v", c)
	}
	if got := d.Regressions(); got != 2 {
		t.Errorf("Regressions() = %d, want 2", got)
	}
	// 3/5 passing before (60%), 2/5 after (40%).
	if got := d.Targets[0].RateDelta(); got != -20 {
		t.Errorf("RateDelta() = %.1f, want -20", got)
	}
}

// Comparing two measurements of different things is how misleading
// numbers get published, so the mismatch is stated rather than hidden.
func TestDiffFlagsIncomparableRuns(t *testing.T) {
	before := reportWith("before", target("llama.cpp/cpu", passing("a")))
	before.ModelDigest = "sha256:aaaa000000000000"
	after := reportWith("after", target("llama.cpp/cpu", passing("a")))
	after.ModelDigest = "sha256:bbbb111111111111"
	after.Temperature = 0.7

	d := Diff(before, after)
	if d.Comparable {
		t.Error("Comparable = true for runs with different weights and sampling")
	}
	joined := strings.Join(d.Mismatches, "; ")
	for _, want := range []string{"model weights", "temperature"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mismatches %q do not mention %q", joined, want)
		}
	}

	// A rebuild keeps name:version while changing contents, so the digest
	// is the only thing that catches "same ref, different unit".
	rebuilt := Diff(
		&Report{ID: "a", Suite: "s", Unit: "u:0.1.0", UnitDigest: "sha256:1111aaaabbbb"},
		&Report{ID: "b", Suite: "s", Unit: "u:0.1.0", UnitDigest: "sha256:2222ccccdddd"},
	)
	if rebuilt.Comparable {
		t.Error("Comparable = true for two different builds of the same ref")
	}
	joined = strings.Join(rebuilt.Mismatches, "; ")
	for _, want := range []string{"unit builds"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mismatches %q do not mention %q", joined, want)
		}
	}
	if !strings.Contains(d.String(), "not directly comparable") {
		t.Error("rendered diff does not warn about comparability")
	}

	same := Diff(reportWith("a", target("llama.cpp/cpu", passing("a"))),
		reportWith("b", target("llama.cpp/cpu", passing("a"))))
	if !same.Comparable {
		t.Errorf("Comparable = false for equivalent runs: %v", same.Mismatches)
	}
}

func TestDiffHandlesTargetsPresentInOnlyOneRun(t *testing.T) {
	before := reportWith("before", target("llama.cpp/cpu", passing("a")))
	after := reportWith("after",
		target("llama.cpp/cpu", passing("a")),
		target("llama.cpp/gpu", failing("a")),
	)

	d := Diff(before, after)
	if len(d.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(d.Targets))
	}
	var gpu *TargetDelta
	for i := range d.Targets {
		if d.Targets[i].Target == "llama.cpp/gpu" {
			gpu = &d.Targets[i]
		}
	}
	if gpu == nil {
		t.Fatal("new target missing from the diff")
	}
	if gpu.OnlyIn != "after" {
		t.Errorf("OnlyIn = %q, want after", gpu.OnlyIn)
	}
	// A case absent from one side must not read as a pass or a failure.
	if gpu.Cases[0].Before != "absent" {
		t.Errorf("before status = %q, want absent", gpu.Cases[0].Before)
	}

	dropped := Diff(after, before)
	for _, td := range dropped.Targets {
		if td.Target == "llama.cpp/gpu" && td.OnlyIn != "before" {
			t.Errorf("dropped target OnlyIn = %q, want before", td.OnlyIn)
		}
	}
	// A target that simply was not evaluated this time is not a
	// regression; only cases that stopped passing are.
	if got := dropped.Regressions(); got != 0 {
		t.Errorf("Regressions() = %d for an unevaluated target, want 0", got)
	}
}

func TestDiffStringShowsOnlyMovement(t *testing.T) {
	before := reportWith("run-a", target("llama.cpp/cpu", passing("quiet"), passing("noisy")))
	after := reportWith("run-b", target("llama.cpp/cpu", passing("quiet"), failing("noisy")))

	out := Diff(before, after).String()
	if !strings.Contains(out, "noisy") {
		t.Errorf("diff omits the case that moved:\n%s", out)
	}
	if strings.Contains(out, "quiet") {
		t.Errorf("diff includes an unchanged case:\n%s", out)
	}
	if !strings.Contains(out, "run-a") || !strings.Contains(out, "run-b") {
		t.Errorf("diff does not name both runs:\n%s", out)
	}
	if !strings.Contains(out, "stopped passing") {
		t.Errorf("diff does not summarise regressions:\n%s", out)
	}
}
