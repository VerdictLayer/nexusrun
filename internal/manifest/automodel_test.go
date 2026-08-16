package manifest

import (
	"strings"
	"testing"
)

const autoUnit = `apiVersion: nexusrun.dev/v1
name: code-reviewer
version: 1.0.0

models:
  - id: main
    profile: default
    context: 8192
    requirements:
      min_context: 8192
      tool_calling: required
      max_size_mb: 3000
      min_quality_score: 11
    candidates:
      - source: ollama:phi3:3.8b
        weight: 1.0
      - source: ollama:llama3.1:8b
        weight: 0.9
      - source: ollama:qwen2.5-coder:7b
        weight: 0.8
    selection_strategy: best_eval_score

entrypoint:
  type: chat
  system_prompt: |
    You are a code reviewer.
`

func TestParseAutoModelProfile(t *testing.T) {
	m, err := Parse([]byte(autoUnit))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mod := m.Models[0]
	if !mod.Auto() {
		t.Fatal("model with candidates should report Auto()")
	}
	if len(mod.Candidates) != 3 {
		t.Errorf("candidates = %d", len(mod.Candidates))
	}
	if mod.Strategy() != StrategyBestEval {
		t.Errorf("strategy = %q", mod.Strategy())
	}
	if mod.ProfileName() != "default" {
		t.Errorf("profile = %q", mod.ProfileName())
	}
	if mod.Requirements.MinQualityScore != 11 || mod.Requirements.MaxSizeMB != 3000 {
		t.Errorf("requirements = %+v", mod.Requirements)
	}
	if mod.Requirements.ToolCalling != ToolCallingRequired {
		t.Errorf("tool_calling = %q", mod.Requirements.ToolCalling)
	}
}

func TestNamedModelStillValidates(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
name: plain
version: 1.0.0
models:
  - source: ollama:llama3.1:8b
    context: 4096
entrypoint:
  type: chat
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("a plain unit must keep working: %v", err)
	}
	if m.Models[0].Auto() {
		t.Error("a model with a source is not an auto profile")
	}
	if m.Models[0].Strategy() != StrategyBestEval {
		t.Errorf("Strategy() should default even for named models, got %q", m.Models[0].Strategy())
	}
}

func TestModelMustBeExactlyOneShape(t *testing.T) {
	both := `apiVersion: nexusrun.dev/v1
name: confused
version: 1.0.0
models:
  - source: ollama:llama3.1:8b
    candidates:
      - source: ollama:phi3:3.8b
entrypoint:
  type: chat
`
	_, err := Parse([]byte(both))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("declaring both a source and candidates should be rejected, got %v", err)
	}

	neither := `apiVersion: nexusrun.dev/v1
name: empty
version: 1.0.0
models:
  - id: main
    context: 4096
entrypoint:
  type: chat
`
	_, err = Parse([]byte(neither))
	if err == nil || !strings.Contains(err.Error(), "source is required") {
		t.Fatalf("a model with neither should be rejected, got %v", err)
	}
}

func TestAutoModelValidationErrors(t *testing.T) {
	base := `apiVersion: nexusrun.dev/v1
name: u
version: 1.0.0
entrypoint:
  type: chat
models:
  - id: main
`
	cases := map[string]struct{ body, want string }{
		"unknown strategy": {
			`    candidates:
      - source: a
    selection_strategy: vibes
`, "selection_strategy"},

		"unknown tool_calling": {
			`    candidates:
      - source: a
    requirements:
      tool_calling: maybe
`, "tool_calling"},

		"candidate without source": {
			`    candidates:
      - weight: 1.0
`, "source is required"},

		"duplicate candidate": {
			`    candidates:
      - source: ollama:phi3:3.8b
      - source: ollama:phi3:3.8b
`, "duplicate candidate"},

		"negative weight": {
			`    candidates:
      - source: a
        weight: -1
`, "weight cannot be negative"},

		// A "passing" strategy with no bar silently ignores quality: every
		// candidate qualifies and the smallest or fastest always wins.
		"fastest_passing without a bar": {
			`    candidates:
      - source: a
      - source: b
    selection_strategy: fastest_passing
`, "min_quality_score"},

		"smallest_passing without a bar": {
			`    candidates:
      - source: a
      - source: b
    selection_strategy: smallest_passing
`, "min_quality_score"},

		"requirements on a named model": {
			`    source: ollama:llama3.1:8b
    requirements:
      min_context: 4096
`, "only apply"},

		"strategy on a named model": {
			`    source: ollama:llama3.1:8b
    selection_strategy: best_eval_score
`, "only applies"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(base + tc.body))
			if err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestPassingStrategiesAcceptedWithABar(t *testing.T) {
	for _, strategy := range []string{StrategyFastestPassing, StrategySmallestPassing} {
		src := `apiVersion: nexusrun.dev/v1
name: u
version: 1.0.0
entrypoint:
  type: chat
models:
  - id: main
    candidates:
      - source: a
      - source: b
    requirements:
      min_quality_score: 11
    selection_strategy: ` + strategy + "\n"
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("%s with a bar should validate: %v", strategy, err)
		}
	}
}

func TestDuplicateModelIDRejected(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
name: u
version: 1.0.0
entrypoint:
  type: chat
models:
  - id: main
    source: a
  - id: main
    source: b
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "duplicate model id") {
		t.Fatalf("expected a duplicate-id error, got %v", err)
	}
}
