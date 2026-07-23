// Package compose chains units into a pipeline: each unit's output
// becomes the next unit's prompt.
//
// This is deliberately the simplest useful composition — a sequential
// pipe, not a DAG. Most real agent workflows are pipelines, and a linear
// chain can be reasoned about, logged, and debugged. Conditional
// branching and fan-out are left until there is a concrete need that a
// pipeline cannot express.
package compose

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Stage is one step in a pipeline.
type Stage struct {
	Unit   string
	Output string
	TokPS  float64
	Took   time.Duration
	Err    error
}

// RunFunc executes a single unit with a prompt and returns its output.
// Injecting it keeps this package independent of how units actually run
// (daemon, direct, or script), and makes the pipeline testable.
type RunFunc func(ctx context.Context, unitRef, prompt string) (output string, tokPerSec float64, err error)

// Options configures a pipeline run.
type Options struct {
	Units    []string
	Input    string
	Progress func(format string, args ...any)
}

// Result is the outcome of a whole pipeline.
type Result struct {
	Stages []Stage
	Output string
	Took   time.Duration
}

// Run executes units in order, piping each output into the next prompt.
func Run(ctx context.Context, run RunFunc, opts Options) (*Result, error) {
	if len(opts.Units) == 0 {
		return nil, fmt.Errorf("no units given")
	}
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}

	res := &Result{}
	start := time.Now()
	current := opts.Input

	for i, u := range opts.Units {
		logf("[%d/%d] %s", i+1, len(opts.Units), u)
		stageStart := time.Now()
		out, tps, err := run(ctx, u, current)
		stage := Stage{Unit: u, Output: out, TokPS: tps, Took: time.Since(stageStart), Err: err}
		res.Stages = append(res.Stages, stage)
		if err != nil {
			res.Took = time.Since(start)
			return res, fmt.Errorf("stage %d (%s) failed: %w", i+1, u, err)
		}
		if strings.TrimSpace(out) == "" {
			res.Took = time.Since(start)
			return res, fmt.Errorf("stage %d (%s) produced no output; nothing to pipe onward", i+1, u)
		}
		logf("      %d chars · %.1f tok/s · %s", len(out), tps, stage.Took.Round(time.Millisecond))
		current = out
	}

	res.Output = current
	res.Took = time.Since(start)
	return res, nil
}
