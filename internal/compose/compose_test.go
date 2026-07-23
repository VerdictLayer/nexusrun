package compose

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPipelinePipesOutputForward(t *testing.T) {
	var seen []string
	run := func(ctx context.Context, unitRef, prompt string) (string, float64, error) {
		seen = append(seen, unitRef+"<-"+prompt)
		return prompt + "|" + unitRef, 10, nil
	}

	res, err := Run(context.Background(), run, Options{
		Units: []string{"a:1", "b:1", "c:1"},
		Input: "start",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if want := "start|a:1|b:1|c:1"; res.Output != want {
		t.Errorf("Output = %q, want %q", res.Output, want)
	}
	if len(res.Stages) != 3 {
		t.Fatalf("got %d stages, want 3", len(res.Stages))
	}
	// Each stage must receive the previous stage's output, not the input.
	if !strings.HasPrefix(seen[1], "b:1<-start|a:1") {
		t.Errorf("stage 2 received %q, want a:1's output", seen[1])
	}
}

func TestPipelineStopsOnError(t *testing.T) {
	calls := 0
	run := func(ctx context.Context, unitRef, prompt string) (string, float64, error) {
		calls++
		if unitRef == "bad:1" {
			return "", 0, errors.New("model missing")
		}
		return "ok", 1, nil
	}

	res, err := Run(context.Background(), run, Options{
		Units: []string{"good:1", "bad:1", "never:1"},
	})
	if err == nil {
		t.Fatal("Run() succeeded, want error")
	}
	if !strings.Contains(err.Error(), "stage 2") || !strings.Contains(err.Error(), "model missing") {
		t.Errorf("error = %v, want it to name stage 2 and the cause", err)
	}
	if calls != 2 {
		t.Errorf("ran %d stages, want 2 (must not continue past a failure)", calls)
	}
	if len(res.Stages) != 2 {
		t.Errorf("recorded %d stages, want 2 including the failed one", len(res.Stages))
	}
}

// An empty output would silently become an empty prompt downstream, so
// the pipeline must stop rather than pipe nothing forward.
func TestPipelineRejectsEmptyOutput(t *testing.T) {
	run := func(ctx context.Context, unitRef, prompt string) (string, float64, error) {
		return "   \n", 0, nil
	}
	_, err := Run(context.Background(), run, Options{Units: []string{"a:1", "b:1"}})
	if err == nil {
		t.Fatal("Run() succeeded on empty output, want error")
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Errorf("error = %v, want it to mention missing output", err)
	}
}

func TestRunRequiresUnits(t *testing.T) {
	run := func(ctx context.Context, unitRef, prompt string) (string, float64, error) {
		return "", 0, nil
	}
	if _, err := Run(context.Background(), run, Options{}); err == nil {
		t.Error("Run() with no units succeeded, want error")
	}
}
