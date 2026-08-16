package workflow

import (
	"strings"
	"testing"
)

func testEnv() Env {
	return Env{
		CondAlways: true,
		"never":    false,
		"input":    "seed",
		"researcher": map[string]any{
			FieldOutput:    "a fairly long research summary about neural processing units",
			FieldTokens:    float64(512),
			FieldTokPerSec: 14.2,
			FieldTookMS:    float64(1200),
			FieldDevice:    "gpu",
			FieldBackend:   "llama.cpp",
			FieldOK:        true,
		},
	}
}

func TestEvalConditions(t *testing.T) {
	env := testEnv()
	cases := []struct {
		expr string
		want bool
	}{
		{"always", true},
		{"never", false},
		{"true", true},
		{"false", false},
		{"len(researcher.output) > 10", true},
		{"len(researcher.output) > 1000", false},
		{"len(researcher.output) >= 60", true},
		{"researcher.tokens == 512", true},
		{"researcher.tokens != 512", false},
		{"researcher.device == \"gpu\"", true},
		{"researcher.device == 'cpu'", false},
		{`contains(researcher.output, "neural")`, true},
		{`contains(researcher.output, "quantum")`, false},
		{`matches(researcher.output, "^a fairly")`, true},
		{`matches(researcher.output, "^zzz")`, false},
		{`lower(researcher.device) == "gpu"`, true},
		{"researcher.tok_per_sec > 10.0", true},
		{"researcher.tok_per_sec > 100", false},
		{"researcher.ok", true},
		{"not researcher.ok", false},
		{"!never", true},
		{"researcher.ok and len(researcher.output) > 10", true},
		{"researcher.ok and len(researcher.output) > 1000", false},
		{"never or researcher.ok", true},
		{"never or never", false},
		{"researcher.ok && researcher.tokens > 100", true},
		{"never || researcher.tokens > 100", true},
		{"(never or always) and researcher.tokens > 1", true},
		{"words(researcher.output) > 5", true},
		{"lines(researcher.output) == 1", true},
		{"researcher.tokens > 100_000", false},
		{"trim(researcher.device) == \"gpu\"", true},
	}
	for _, tc := range cases {
		got, err := Eval(tc.expr, env)
		if err != nil {
			t.Errorf("%s: %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestEvalShortCircuitsOnAbsentAgent(t *testing.T) {
	env := testEnv()
	// "writer" never ran. An `and` guarded by its .ok must not error.
	got, err := Eval("writer.ok and len(writer.output) > 5", env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("condition on an agent that never ran should be false")
	}
}

func TestEvalRejectsMalformed(t *testing.T) {
	env := testEnv()
	for _, expr := range []string{
		"",
		"len(",
		"len(researcher.output",
		"researcher.output >",
		"(always",
		"always)",
		`"unterminated`,
		"1 2",
		"researcher.output == ",
		"$$$",
	} {
		if _, err := Eval(expr, env); err == nil {
			t.Errorf("%q should not parse", expr)
		}
	}
}

func TestEvalRejectsIncomparableTypes(t *testing.T) {
	env := testEnv()
	if _, err := Eval(`researcher.output > "x"`, env); err == nil {
		t.Error("comparing two strings with > should be an error, not a silent false")
	}
}

func TestCheckConditionValidatesNames(t *testing.T) {
	agents := []string{"researcher", "writer"}

	for _, expr := range []string{
		"always",
		"len(researcher.output) > 200",
		`contains(writer.output, "x") and researcher.ok`,
	} {
		if err := CheckCondition(expr, agents); err != nil {
			t.Errorf("%q should validate: %v", expr, err)
		}
	}

	for expr, want := range map[string]string{
		"len(ghost.output) > 1":         "ghost",
		"researcher.putput == \"x\"":    "field",
		"frobnicate(researcher.output)": "unknown function",
		"researcher":                    "agent",
		"nosuchname":                    "unknown name",
		"len(researcher.output, 2) > 1": "argument",
		"researcher.output.deep.deeper": "nested",
	} {
		err := CheckCondition(expr, agents)
		if err == nil {
			t.Errorf("%q should fail validation", expr)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q should mention %q", expr, err, want)
		}
	}
}

func TestWordOperatorsDoNotSplitIdentifiers(t *testing.T) {
	// An agent named "android" begins with "and"; an agent named "organ"
	// begins with "or". Neither may be read as an operator.
	env := Env{
		"android": map[string]any{FieldOK: true, FieldOutput: "x"},
		"organ":   map[string]any{FieldOK: true, FieldOutput: "y"},
		"notes":   map[string]any{FieldOK: true, FieldOutput: "z"},
	}
	for _, expr := range []string{"android.ok", "organ.ok", "notes.ok"} {
		got, err := Eval(expr, env)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if !got {
			t.Errorf("%s should be true", expr)
		}
	}
}

func TestStringEscapes(t *testing.T) {
	env := Env{"a": map[string]any{FieldOutput: "line1\nline2"}}
	got, err := Eval(`contains(a.output, "1\nline")`, env)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("escape sequence not handled")
	}
}
