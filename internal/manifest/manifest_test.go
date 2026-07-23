package manifest

import (
	"strings"
	"testing"
)

const valid = `apiVersion: nexusrun.dev/v1
name: research-agent
version: 0.2.0
models:
  - id: main
    source: ollama:llama3.1:8b
entrypoint:
  type: chat
`

func TestParseValid(t *testing.T) {
	m, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if m.Ref() != "research-agent:0.2.0" {
		t.Errorf("Ref() = %q, want research-agent:0.2.0", m.Ref())
	}
	if got := m.Hardware.Prefer; len(got) != 3 || got[0] != "npu" {
		t.Errorf("default Prefer = %v, want [npu gpu cpu]", got)
	}
	if m.Models[0].Format != "gguf" {
		t.Errorf("default Format = %q, want gguf", m.Models[0].Format)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{
			"wrong apiVersion",
			"apiVersion: v1\nname: a\nversion: 1\nmodels:\n  - source: x\n",
			"apiVersion",
		},
		{
			"uppercase name",
			"apiVersion: nexusrun.dev/v1\nname: MyAgent\nversion: 1\nmodels:\n  - source: x\n",
			"lowercase",
		},
		{
			"no models for chat unit",
			"apiVersion: nexusrun.dev/v1\nname: a\nversion: 1\n",
			"at least one model",
		},
		{
			"model without source",
			"apiVersion: nexusrun.dev/v1\nname: a\nversion: 1\nmodels:\n  - id: main\n",
			"source is required",
		},
		{
			"unknown accelerator",
			"apiVersion: nexusrun.dev/v1\nname: a\nversion: 1\nmodels:\n  - source: x\nhardware:\n  prefer: [tpu]\n",
			"unknown accelerator",
		},
		{
			"unknown field is not silently ignored",
			"apiVersion: nexusrun.dev/v1\nname: a\nversion: 1\nmodels:\n  - source: x\nmodls: typo\n",
			"field modls",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("Parse() succeeded, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestScriptUnitNeedsNoModel(t *testing.T) {
	y := "apiVersion: nexusrun.dev/v1\nname: tool-only\nversion: 1.0.0\nentrypoint:\n  type: script\n  script: main.py\n"
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatalf("script unit rejected: %v", err)
	}
}
