package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validSuite = `apiVersion: nexusrun.dev/v1
kind: EvalSuite
name: triage-basics
max_tokens: 128
cases:
  - name: extracts-error-code
    prompt: "Which error code is in this log line: [warn] upstream E_TIMEOUT after 30s"
    expect:
      contains: ["E_TIMEOUT"]
      not_contains: ["I cannot"]
`

func TestParseValidSuite(t *testing.T) {
	s, err := Parse([]byte(validSuite))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if s.Name != "triage-basics" {
		t.Errorf("Name = %q, want %q", s.Name, "triage-basics")
	}
	if len(s.Cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(s.Cases))
	}
	// Temperature is left nil so the runner can apply its own default of
	// 0; a suite that never mentions sampling must not silently get 0.7.
	if s.Temperature != nil {
		t.Errorf("Temperature = %v, want nil when unset", *s.Temperature)
	}
}

func TestParseRejectsBadSuites(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{
			name: "wrong apiVersion",
			yaml: strings.Replace(validSuite, "nexusrun.dev/v1", "nexusrun.dev/v99", 1),
			// A future schema must not be interpreted by today's rules.
			wantErr: "apiVersion",
		},
		{
			name:    "wrong kind",
			yaml:    strings.Replace(validSuite, "kind: EvalSuite", "kind: Unit", 1),
			wantErr: "kind",
		},
		{
			// A mistyped assertion key would otherwise become a case with
			// nothing to check — which passes, silently inflating scores.
			name:    "unknown field",
			yaml:    strings.Replace(validSuite, "      contains:", "      contains_:", 1),
			wantErr: "field contains_",
		},
		{
			name:    "no cases",
			yaml:    "apiVersion: nexusrun.dev/v1\nkind: EvalSuite\nname: empty\ncases: []\n",
			wantErr: "at least one case",
		},
		{
			name: "case without assertions",
			yaml: `apiVersion: nexusrun.dev/v1
kind: EvalSuite
name: nothing-asserted
cases:
  - name: vacuous
    prompt: "hello"
    expect: {}
`,
			wantErr: "at least one assertion",
		},
		{
			name: "duplicate case names",
			yaml: `apiVersion: nexusrun.dev/v1
kind: EvalSuite
name: dupes
cases:
  - name: same
    prompt: "a"
    expect: {contains: ["x"]}
  - name: same
    prompt: "b"
    expect: {contains: ["y"]}
`,
			wantErr: "duplicate case name",
		},
		{
			// Catching this before the suite runs is the whole reason
			// Validate compiles patterns: the alternative is discovering
			// it after twenty minutes of generation.
			name: "invalid regex",
			yaml: `apiVersion: nexusrun.dev/v1
kind: EvalSuite
name: bad-regex
cases:
  - name: broken
    prompt: "a"
    expect: {regex: ["E_[A-Z"]}
`,
			wantErr: "invalid regex",
		},
		{
			name: "temperature out of range",
			yaml: `apiVersion: nexusrun.dev/v1
kind: EvalSuite
name: too-hot
temperature: 5
cases:
  - name: c
    prompt: "a"
    expect: {contains: ["x"]}
`,
			wantErr: "outside 0–2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("Parse() succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Parse() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	write := func(t *testing.T, dir, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, DirName), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, DirName, name), []byte(validSuite), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("single suite of any name", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "quality.yaml")
		got, err := Discover(dir)
		if err != nil {
			t.Fatalf("Discover() error = %v", err)
		}
		if filepath.Base(got) != "quality.yaml" {
			t.Errorf("Discover() = %q, want quality.yaml", got)
		}
	})

	t.Run("suite.yaml wins among several", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "aaa.yaml")
		write(t, dir, "suite.yaml")
		got, err := Discover(dir)
		if err != nil {
			t.Fatalf("Discover() error = %v", err)
		}
		if filepath.Base(got) != "suite.yaml" {
			t.Errorf("Discover() = %q, want suite.yaml", got)
		}
	})

	// Ambiguity must not be resolved by guessing: which suite produced a
	// score has to be unambiguous.
	t.Run("ambiguous set is an error naming the candidates", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "aaa.yaml")
		write(t, dir, "bbb.yaml")
		_, err := Discover(dir)
		if err == nil {
			t.Fatal("Discover() succeeded on an ambiguous directory")
		}
		for _, want := range []string{"aaa.yaml", "bbb.yaml", "--suite"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %v does not mention %q", err, want)
			}
		}
	})

	t.Run("no evals directory", func(t *testing.T) {
		if _, err := Discover(t.TempDir()); err == nil {
			t.Fatal("Discover() succeeded with no evals/ directory")
		}
	})
}
