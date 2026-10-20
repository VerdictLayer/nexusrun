package workflow

import (
	"strings"
	"testing"
)

const head = "apiVersion: nexusrun.dev/v1\nkind: Workflow\nname: demo\nversion: 0.1.0\n"

func paths(ds []Diagnostic) map[string]Diagnostic {
	m := map[string]Diagnostic{}
	for _, d := range ds {
		m[d.Path] = d
	}
	return m
}

func TestDiagnoseLocatesEachProblem(t *testing.T) {
	spec, ds := Diagnose([]byte(head + `agents:
  writer: {unit: ""}
  critic: {unit: critic:1.0.0, depends_on: [ghost]}
routing:
  - {from: writer, to: critic, condition: "writer.nope == 1"}
  - {from: writer, to: nobody}
shared_state: {backend: memory, path: x}
`))
	if spec == nil {
		t.Fatal("a structurally readable file must still yield a spec to draw")
	}
	got := paths(ds)
	for _, want := range []string{
		"agents.writer.unit", "agents.critic.depends_on",
		"routing[0].condition", "routing[1].to", "shared_state.path",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing a diagnostic at %s; got %v", want, ds)
		}
	}
	if d := got["routing[0].condition"]; len(d.Agents) != 2 || d.Agents[0] != "writer" || d.Agents[1] != "critic" {
		t.Errorf("a route problem must name the edge's agents, got %v", d.Agents)
	}
}

func TestDiagnoseReportsCycleMembers(t *testing.T) {
	_, ds := Diagnose([]byte(head + `agents:
  a: {unit: a:1}
  b: {unit: b:1}
  c: {unit: c:1}
routing:
  - {from: a, to: b}
  - {from: b, to: a}
`))
	if len(ds) != 1 || ds[0].Path != "agents" || !strings.Contains(ds[0].Message, "cycle") {
		t.Fatalf("want one cycle diagnostic, got %v", ds)
	}
	if got := strings.Join(ds[0].Agents, ","); got != "a,b" {
		t.Errorf("cycle members = %q, want a,b", got)
	}
}

func TestDiagnoseReadsBrokenYAMLWithLines(t *testing.T) {
	_, ds := Diagnose([]byte(head + "agents:\n  a: {unit: a:1}\nbogus: true\n"))
	if len(ds) == 0 || ds[0].Line != 7 || !strings.Contains(ds[0].Message, `unknown field "bogus"`) {
		t.Fatalf("want an unknown-field diagnostic on line 7, got %+v", ds)
	}
	spec, ds := Diagnose([]byte("agents: [unterminated\n"))
	if spec != nil || len(ds) != 1 || ds[0].Line == 0 {
		t.Fatalf("syntax error must carry a line and no spec, got %v %+v", spec, ds)
	}
}

func TestDiagnoseValidFileIsClean(t *testing.T) {
	spec, ds := Diagnose([]byte(head + "agents:\n  a: {unit: a:1}\n  b: {unit: b:1}\nrouting:\n  - {from: a, to: b}\n"))
	if len(ds) != 0 || spec == nil || spec.SharedState.Backend != StateMemory {
		t.Fatalf("a valid file has no diagnostics and gets defaults, got %v %+v", ds, spec)
	}
}
