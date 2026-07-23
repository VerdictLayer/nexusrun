// Package bench measures real generation throughput across every device
// a host can actually execute on.
//
// This is the empirical half of NexusRun's hardware story. Detection and
// capability probing say what *should* work; bench says what is actually
// faster on this specific machine, with this specific model and backend
// build. The two disagree more often than people expect — a CPU-only
// llama.cpp build on a GPU workstation, a broken driver, an NPU whose
// execution provider is missing, or a small model where accelerator
// transfer overhead exceeds the compute win.
package bench

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lanceseidman/nexusrun/internal/engine"
	"github.com/lanceseidman/nexusrun/internal/hardware"
)

// Options configures a benchmark sweep.
type Options struct {
	ModelPath string
	Prompt    string
	MaxTokens int
	Runs      int      // repetitions per device; the median is reported
	Devices   []string // restrict to these classes; empty means all usable
	Progress  func(format string, args ...any)
}

// DeviceResult is one device's measured performance.
type DeviceResult struct {
	Device    string        `json:"device"`
	Backend   string        `json:"backend"`
	EvalTPS   float64       `json:"eval_tokens_per_sec"`
	PromptTPS float64       `json:"prompt_tokens_per_sec"`
	LoadMS    float64       `json:"load_ms"`
	TokensOut int           `json:"tokens_out"`
	Wall      time.Duration `json:"wall_time"`
	Error     string        `json:"error,omitempty"`
}

// Report is the full sweep result.
type Report struct {
	Model     string           `json:"model"`
	Hardware  *hardware.Report `json:"hardware"`
	Results   []DeviceResult   `json:"results"`
	Fastest   string           `json:"fastest,omitempty"`
	Speedup   float64          `json:"speedup_vs_cpu,omitempty"`
	Timestamp time.Time        `json:"timestamp"`
}

// Run sweeps every usable device and returns measured throughput.
func Run(ctx context.Context, opts Options) (*Report, error) {
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.Runs <= 0 {
		opts.Runs = 3
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 64
	}
	if opts.Prompt == "" {
		opts.Prompt = "Explain what a neural processing unit does, in two sentences."
	}

	hw := hardware.Detect()
	rep := &Report{Model: opts.ModelPath, Hardware: hw, Timestamp: time.Now()}

	// Build the candidate set from backend capability, not hardware
	// presence: a device nothing can execute on is not a candidate.
	type candidate struct {
		device  string
		backend engine.Backend
	}
	var candidates []candidate
	seen := map[string]bool{}
	for _, b := range engine.All() {
		cap := b.Probe()
		if !cap.Available {
			logf("skip %s — %s", b.Name(), cap.Detail)
			continue
		}
		if !cap.AcceptsModelPath {
			logf("skip %s — runs models by name, not by path", b.Name())
			continue
		}
		for _, d := range cap.Devices {
			if !hw.Has(d) {
				logf("skip %s/%s — backend supports it but no such device detected", b.Name(), d)
				continue
			}
			if len(opts.Devices) > 0 && !contains(opts.Devices, d) {
				continue
			}
			key := b.Name() + "/" + d
			if seen[key] {
				continue
			}
			seen[key] = true
			candidates = append(candidates, candidate{device: d, backend: b})
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no usable device/backend pair found; run `nexus doctor` for details")
	}

	for _, c := range candidates {
		logf("benchmarking %s on %s (%d runs)…", c.backend.Name(), strings.ToUpper(c.device), opts.Runs)
		var samples []engine.Result
		var lastErr error
		for i := 0; i < opts.Runs; i++ {
			res, err := c.backend.Generate(ctx, engine.Request{
				ModelPath:   opts.ModelPath,
				Prompt:      opts.Prompt,
				MaxTokens:   opts.MaxTokens,
				Temperature: 0,
				Device:      c.device,
			})
			if err != nil {
				lastErr = err
				break
			}
			samples = append(samples, *res)
		}
		dr := DeviceResult{Device: c.device, Backend: c.backend.Name()}
		if lastErr != nil || len(samples) == 0 {
			if lastErr != nil {
				dr.Error = lastErr.Error()
			} else {
				dr.Error = "no samples collected"
			}
			rep.Results = append(rep.Results, dr)
			logf("  failed: %s", dr.Error)
			continue
		}
		med := medianBy(samples, func(r engine.Result) float64 { return r.EvalTPS })
		dr.EvalTPS = med.EvalTPS
		dr.PromptTPS = med.PromptTPS
		dr.LoadMS = med.LoadMS
		dr.TokensOut = med.TokensOut
		dr.Wall = med.WallTime
		rep.Results = append(rep.Results, dr)
		logf("  %.2f tok/s (median of %d)", dr.EvalTPS, len(samples))
	}

	// Rank, and compute the accelerator speedup against CPU baseline.
	sort.SliceStable(rep.Results, func(i, j int) bool {
		return rep.Results[i].EvalTPS > rep.Results[j].EvalTPS
	})
	var cpuTPS float64
	for _, r := range rep.Results {
		if r.Device == hardware.ClassCPU && r.Error == "" {
			cpuTPS = r.EvalTPS
		}
	}
	if len(rep.Results) > 0 && rep.Results[0].Error == "" {
		rep.Fastest = rep.Results[0].Backend + "/" + rep.Results[0].Device
		if cpuTPS > 0 {
			rep.Speedup = rep.Results[0].EvalTPS / cpuTPS
		}
	}
	return rep, nil
}

// String renders the report as a readable table.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Model:    %s\n", r.Model)
	fmt.Fprintf(&b, "Host:     %s/%s · %s\n\n", r.Hardware.OS, r.Hardware.Arch, r.Hardware.CPUModel)
	fmt.Fprintf(&b, "  %-8s %-14s %10s %10s %9s\n", "DEVICE", "BACKEND", "EVAL tok/s", "PROMPT t/s", "LOAD ms")
	fmt.Fprintf(&b, "  %s\n", strings.Repeat("─", 56))
	for _, res := range r.Results {
		if res.Error != "" {
			fmt.Fprintf(&b, "  %-8s %-14s %s\n", strings.ToUpper(res.Device), res.Backend, "failed: "+firstLine(res.Error))
			continue
		}
		fmt.Fprintf(&b, "  %-8s %-14s %10.2f %10.2f %9.0f\n",
			strings.ToUpper(res.Device), res.Backend, res.EvalTPS, res.PromptTPS, res.LoadMS)
	}
	if r.Fastest != "" {
		fmt.Fprintf(&b, "\nFastest: %s", r.Fastest)
		if r.Speedup > 0 && r.Speedup != 1 {
			fmt.Fprintf(&b, " (%.2fx vs CPU)", r.Speedup)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func medianBy(rs []engine.Result, key func(engine.Result) float64) engine.Result {
	sorted := make([]engine.Result, len(rs))
	copy(sorted, rs)
	sort.SliceStable(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	return sorted[len(sorted)/2]
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
