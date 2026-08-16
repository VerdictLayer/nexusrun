package workflow

import (
	"strings"
	"testing"
)

const validSpec = `apiVersion: nexusrun.dev/v1
kind: Workflow
name: content-pipeline
version: 1.0.0

agents:
  researcher:
    unit: ghcr.io/acme/researcher:v1.2.0
    model: ollama:llama3.1:8b
    hardware:
      prefer: [gpu, cpu]
    env:
      MAX_DEPTH: "3"
  writer:
    unit: ghcr.io/acme/writer:v2.0.0
    depends_on: [researcher]
  editor:
    unit: ghcr.io/acme/editor:v1.0.0

routing:
  - from: researcher
    to: writer
    condition: "len(researcher.output) > 200"
    transform: |
      {{ summarize 100 .researcher.output }}
  - from: writer
    to: editor
    condition: "always"

shared_state:
  backend: memory

network:
  isolation: process
  allow_loopback: true
`

func TestParseValidSpec(t *testing.T) {
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Ref() != "content-pipeline:1.0.0" {
		t.Errorf("ref = %q", s.Ref())
	}
	if len(s.Agents) != 3 {
		t.Errorf("agents = %d, want 3", len(s.Agents))
	}
	if got := s.Agents["researcher"].Env["MAX_DEPTH"]; got != "3" {
		t.Errorf("env MAX_DEPTH = %q", got)
	}
	if s.SharedState.Backend != StateMemory {
		t.Errorf("state backend = %q", s.SharedState.Backend)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	src := strings.Replace(validSpec, "  editor:", "  editor:\n    typo_field: x", 1)
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("expected an unknown-field error")
	}
}

func TestOrderIsTopologicalAndStable(t *testing.T) {
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	order, err := s.Order()
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	if pos["researcher"] > pos["writer"] || pos["writer"] > pos["editor"] {
		t.Errorf("order %v does not respect dependencies", order)
	}

	// Order must be a property of the file, not of map iteration.
	for i := 0; i < 20; i++ {
		again, err := s.Order()
		if err != nil {
			t.Fatal(err)
		}
		for j := range order {
			if again[j] != order[j] {
				t.Fatalf("order is unstable: %v then %v", order, again)
			}
		}
	}
}

func TestRoutingImpliesDependency(t *testing.T) {
	// editor declares no depends_on; only the route makes it downstream.
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	deps := s.Edges()
	if len(deps["editor"]) != 1 || deps["editor"][0] != "writer" {
		t.Errorf("editor deps = %v, want [writer]", deps["editor"])
	}
}

func TestCycleIsRejectedWithAPath(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: loop
version: 1.0.0
agents:
  a: {unit: a:1}
  b: {unit: b:1}
  c: {unit: c:1}
routing:
  - {from: a, to: b}
  - {from: b, to: c}
  - {from: c, to: a}
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error does not mention a cycle: %v", err)
	}
	// The report must name a followable path, not just a set.
	if !strings.Contains(err.Error(), "→") {
		t.Errorf("cycle error should show the path: %v", err)
	}
}

func TestValidationCatchesUnknownAgentReferences(t *testing.T) {
	cases := map[string]string{
		"route from": `routing:
  - {from: ghost, to: a}`,
		"route to": `routing:
  - {from: a, to: ghost}`,
		"depends_on": `  b:
    unit: b:1
    depends_on: [ghost]`,
	}
	base := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: w
version: 1.0.0
agents:
  a: {unit: a:1}
`
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(base + extra))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "ghost") {
				t.Errorf("error should name the missing agent: %v", err)
			}
		})
	}
}

func TestValidationCatchesBadConditionsAndTransforms(t *testing.T) {
	base := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: w
version: 1.0.0
agents:
  a: {unit: a:1}
  b: {unit: b:1}
routing:
  - from: a
    to: b
`
	for name, tc := range map[string]struct{ extra, want string }{
		"typo in field":     {`    condition: "len(a.putput) > 5"`, "field"},
		"unknown agent":     {`    condition: "len(ghost.output) > 5"`, "ghost"},
		"unknown function":  {`    condition: "frobnicate(a.output)"`, "unknown function"},
		"unparseable":       {`    condition: "len(a.output >"`, ""},
		"bad transform":     {"    transform: \"{{ summarize 10 }\"", ""},
		"unknown transform": {`    transform: "{{ nosuchfunc .a.output }}"`, "nosuchfunc"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(base + tc.extra + "\n"))
			if err == nil {
				t.Fatalf("expected an error for %s", name)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestSQLiteBackendExplainsItself(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: w
version: 1.0.0
agents:
  a: {unit: a:1}
shared_state:
  backend: sqlite
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected sqlite to be rejected")
	}
	// A bare "unsupported" would leave the reader with no next step.
	if !strings.Contains(err.Error(), StateFile) {
		t.Errorf("error should point at the file backend: %v", err)
	}
}

func TestDuplicateRouteRejected(t *testing.T) {
	src := `apiVersion: nexusrun.dev/v1
kind: Workflow
name: w
version: 1.0.0
agents:
  a: {unit: a:1}
  b: {unit: b:1}
routing:
  - {from: a, to: b}
  - {from: a, to: b, condition: "always"}
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "duplicate route") {
		t.Fatalf("expected a duplicate-route error, got %v", err)
	}
}

func TestSourcesAndSinks(t *testing.T) {
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Sources(); len(got) != 1 || got[0] != "researcher" {
		t.Errorf("sources = %v, want [researcher]", got)
	}
	if got := s.Sinks(); len(got) != 1 || got[0] != "editor" {
		t.Errorf("sinks = %v, want [editor]", got)
	}
}

func TestKindMustBeWorkflow(t *testing.T) {
	src := strings.Replace(validSpec, "kind: Workflow", "kind: Unit", 1)
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("expected a kind error, got %v", err)
	}
}
