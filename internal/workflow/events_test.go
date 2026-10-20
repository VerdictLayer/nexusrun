package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func kinds(evs []Event) string {
	var out []string
	for _, e := range evs {
		s := e.Kind
		switch e.Kind {
		case "route":
			s += fmt.Sprintf(":%s>%s=%v", e.From, e.To, *e.Fired)
		case "agent_start", "agent_done", "agent_skipped", "agent_failed":
			s += ":" + e.Agent
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestRunReportsEachStepInOrder(t *testing.T) {
	var calls []AgentRequest
	var evs []Event
	_, err := Run(context.Background(), stubExec(map[string]string{"first": "one", "second": "two", "third": "three"}, &calls),
		RunOptions{Spec: mustParse(t, linear), Input: "go", Events: func(e Event) { evs = append(evs, e) }})
	if err != nil {
		t.Fatal(err)
	}
	want := "start agent_start:first agent_done:first route:first>second=true agent_start:second agent_done:second route:second>third=true agent_start:third agent_done:third done"
	if got := kinds(evs); got != want {
		t.Errorf("events:\n got %s\nwant %s", got, want)
	}
	done := evs[len(evs)-1]
	if done.OK == nil || !*done.OK {
		t.Errorf("done = %+v", done)
	}
	d := evs[2]
	if d.Output != "one" || d.Tokens != 3 || d.TokPerSec != 12.5 || d.Backend != "stub" || d.Device != "cpu" {
		t.Errorf("agent_done carries the result: %+v", d)
	}
	if evs[0].Order[0] != "first" || len(evs[0].Order) != 3 {
		t.Errorf("start lists the order: %+v", evs[0])
	}
}

func TestRunReportsABlockedRouteAndSkippedAgent(t *testing.T) {
	spec := mustParse(t, `apiVersion: nexusrun.dev/v1
kind: Workflow
name: gate
version: 1.0.0
agents:
  a: {unit: a:1}
  b: {unit: b:1}
routing:
  - {from: a, to: b, condition: "len(a.output) > 100"}
`)
	var calls []AgentRequest
	var evs []Event
	if _, err := Run(context.Background(), stubExec(map[string]string{"a": "short"}, &calls),
		RunOptions{Spec: spec, Events: func(e Event) { evs = append(evs, e) }}); err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(evs), "start agent_start:a agent_done:a route:a>b=false agent_skipped:b done"; got != want {
		t.Errorf("events:\n got %s\nwant %s", got, want)
	}
	if evs[4].Reason == "" {
		t.Error("a skipped agent says why")
	}
}

func TestRunReportsAFailureThenDone(t *testing.T) {
	var calls []AgentRequest
	var evs []Event
	_, err := Run(context.Background(), stubExec(map[string]string{"first": "one"}, &calls),
		RunOptions{Spec: mustParse(t, linear), Events: func(e Event) { evs = append(evs, e) }})
	if err == nil {
		t.Fatal("the second agent has no stub output and must fail")
	}
	got := kinds(evs)
	if !strings.Contains(got, "agent_failed:second") || !strings.HasSuffix(got, " done") {
		t.Errorf("events: %s", got)
	}
	if last := evs[len(evs)-1]; last.OK == nil || *last.OK || last.Error == "" {
		t.Errorf("done must report the failure: %+v", last)
	}
}
