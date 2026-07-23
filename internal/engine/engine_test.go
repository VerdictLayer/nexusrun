package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lanceseidman/nexusrun/internal/hardware"
)

// stubBackend exercises accelerator scheduling on a machine that has no
// accelerator. Without it the NPU and GPU paths are unreachable in tests.
type stubBackend struct{ cap Capability }

func (s *stubBackend) Name() string      { return s.cap.Backend }
func (s *stubBackend) Probe() Capability { return s.cap }
func (s *stubBackend) Generate(context.Context, Request) (*Result, error) {
	return nil, nil
}

func reportWith(classes ...string) *hardware.Report {
	r := &hardware.Report{}
	for _, c := range classes {
		r.Devices = append(r.Devices, hardware.Device{Class: c})
	}
	return r
}

func ready(name string, devices ...string) Capability {
	return Capability{Backend: name, Available: true, AcceptsModelPath: true, Devices: devices}
}

// The project's core rule: schedule on the intersection of detected
// hardware and probed backend capability, never on detection alone.
func TestSelectFromUsesIntersection(t *testing.T) {
	tests := []struct {
		name        string
		backends    []Backend
		hw          *hardware.Report
		prefer      []string
		wantBackend string
		wantDevice  string
	}{
		{
			name:        "prefers the first usable accelerator",
			backends:    []Backend{&stubBackend{ready("acc", "npu", "cpu")}},
			hw:          reportWith("npu", "cpu"),
			prefer:      []string{"npu", "gpu", "cpu"},
			wantBackend: "acc", wantDevice: "npu",
		},
		{
			name: "GPU detected but undrivable falls back to CPU",
			// The development machine exactly: a real GPU, and a
			// llama.cpp build that cannot touch it.
			backends:    []Backend{&stubBackend{ready("llama.cpp", "cpu")}},
			hw:          reportWith("gpu", "cpu"),
			prefer:      []string{"gpu", "cpu"},
			wantBackend: "llama.cpp", wantDevice: "cpu",
		},
		{
			name:        "backend claims a device the host lacks",
			backends:    []Backend{&stubBackend{ready("acc", "npu", "cpu")}},
			hw:          reportWith("cpu"),
			prefer:      []string{"npu", "cpu"},
			wantBackend: "acc", wantDevice: "cpu",
		},
		{
			name: "skips backends that cannot take a model path",
			backends: []Backend{
				&stubBackend{Capability{Backend: "ollama", Available: true, Devices: []string{"gpu"}}},
				&stubBackend{ready("llama.cpp", "cpu")},
			},
			hw:          reportWith("gpu", "cpu"),
			prefer:      []string{"gpu", "cpu"},
			wantBackend: "llama.cpp", wantDevice: "cpu",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, dev, err := SelectFrom(tt.backends, tt.hw, tt.prefer)
			if err != nil {
				t.Fatalf("SelectFrom: %v", err)
			}
			if b.Name() != tt.wantBackend || dev != tt.wantDevice {
				t.Errorf("got (%s, %s), want (%s, %s)", b.Name(), dev, tt.wantBackend, tt.wantDevice)
			}
		})
	}
}

// "no backend available" alone is useless; the error must name every
// backend and why each one is out. `nexus doctor` exists for this reason.
func TestSelectFromErrorNamesEveryReason(t *testing.T) {
	_, _, err := SelectFrom([]Backend{
		&stubBackend{Capability{Backend: "llama.cpp", Detail: "llama-cli not found in PATH"}},
		&stubBackend{Capability{Backend: "onnxruntime", Detail: "shared library not found"}},
	}, reportWith("cpu"), []string{"cpu"})
	if err == nil {
		t.Fatal("expected an error when no backend is available")
	}
	for _, want := range []string{"llama.cpp", "llama-cli not found in PATH", "onnxruntime", "shared library not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q; it must say what to do next\ngot: %v", want, err)
		}
	}
}

// The web console (internal/server/console.html) and `nexus doctor --json`
// read these exact keys. Capability shipped untagged once, which rendered
// every backend row in the console as an "OFF" pill labelled "undefined" —
// the one panel that demonstrates capability probing, showing nothing.
func TestCapabilityJSONKeys(t *testing.T) {
	data, err := json.Marshal(Capability{
		Backend:          "llama.cpp",
		Available:        true,
		Devices:          []string{"cpu"},
		Version:          "5688",
		Detail:           "CPU-only build",
		AcceptsModelPath: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"backend", "available", "devices", "version", "detail", "accepts_model_path"} {
		if _, ok := got[key]; !ok {
			t.Errorf("Capability JSON is missing %q; console.html reads it directly\ngot: %s", key, data)
		}
	}
}

// llama-cli prints "[end of text]" on stdout after the EOS token. Left
// in, it was saved to run logs and piped into the next `nexus compose`
// stage, which made the model echo corrupted variants of it back.
func TestCleanGenerated(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello [end of text]", "hello"},
		{"hello [end of text]\n\n\n", "hello"},
		{"  hello  ", "hello"},
		{"hello", "hello"},
		{"", ""},
		// A marker mid-text is model output, not decoration, and stays.
		{"a [end of text] b", "a [end of text] b"},
		// llama-cli occasionally repeats it.
		{"hi [end of text] [end of text]", "hi"},
	}
	for _, c := range cases {
		if got := cleanGenerated(c.in); got != c.want {
			t.Errorf("cleanGenerated(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// `nexus run --json` encodes engine.Result on the direct path and
// daemon.RunResponse when a warm pool answers. The two must agree on
// casing, or the same flag returns two different shapes.
func TestResultJSONKeys(t *testing.T) {
	data, err := json.Marshal(Result{
		Text:      "hi",
		Backend:   "llama.cpp",
		Device:    "cpu",
		TokensOut: 12,
		EvalTPS:   14.4,
		WallTime:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	// Names match daemon.RunResponse and bench.DeviceResult.
	for _, key := range []string{"text", "backend", "device", "tokens_out", "eval_tokens_per_sec", "prompt_tokens_per_sec"} {
		if _, ok := got[key]; !ok {
			t.Errorf("Result JSON is missing %q\ngot: %s", key, data)
		}
	}
}
