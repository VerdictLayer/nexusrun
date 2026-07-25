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

const validWithTool = `apiVersion: nexusrun.dev/v1
name: notes-agent
version: 0.1.0
models:
  - source: ollama:llama3.1:8b
entrypoint:
  type: chat
capabilities: [storage]
tools:
  - name: search_notes
    description: Search the user's notes for a phrase
    parameters:
      type: object
      properties:
        query:
          type: string
      required: [query]
    exec:
      type: script
      command: ["python3", "tools/search.py"]
    capabilities: [storage]
`

func TestParseToolDeclaration(t *testing.T) {
	m, err := Parse([]byte(validWithTool))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(m.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(m.Tools))
	}
	tool := m.Tools[0]
	if tool.Name != "search_notes" {
		t.Errorf("Name = %q, want search_notes", tool.Name)
	}
	if tool.Exec.Type != ToolExecScript || len(tool.Exec.Command) != 2 {
		t.Errorf("Exec = %+v, want a two-word script command", tool.Exec)
	}
	// The schema is passed to the model untouched, so it must survive
	// parsing as-is rather than being flattened into a Go type.
	if typ, _ := tool.Parameters["type"].(string); typ != "object" {
		t.Errorf("Parameters = %+v, want the JSON Schema preserved", tool.Parameters)
	}
	if _, ok := tool.Parameters["properties"]; !ok {
		t.Error("Parameters lost its properties")
	}
}

func TestToolValidation(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{
			name:    "missing description",
			yaml:    strings.Replace(validWithTool, "    description: Search the user's notes for a phrase\n", "", 1),
			wantErr: "description is required",
		},
		{
			name:    "missing exec type",
			yaml:    strings.Replace(validWithTool, "      type: script\n", "", 1),
			wantErr: "exec.type is required",
		},
		{
			name:    "unknown exec type",
			yaml:    strings.Replace(validWithTool, "      type: script", "      type: grpc", 1),
			wantErr: `exec.type must be "script"`,
		},
		{
			name:    "script tool without a command",
			yaml:    strings.Replace(validWithTool, `      command: ["python3", "tools/search.py"]`+"\n", "", 1),
			wantErr: "exec.command is required",
		},
		{
			// The model addresses tools by name; two with one name leaves
			// no way to say which was called.
			name:    "duplicate names",
			yaml:    validWithTool + strings.SplitN(validWithTool, "tools:\n", 2)[1],
			wantErr: "duplicate tool name",
		},
		{
			name:    "name the backends would reject",
			yaml:    strings.Replace(validWithTool, "name: search_notes", "name: search notes!", 1),
			wantErr: "name must be 1–64 characters",
		},
		{
			name:    "parameters that are not an object schema",
			yaml:    strings.Replace(validWithTool, "      type: object", "      type: string", 1),
			wantErr: "parameters must be a JSON Schema object",
		},
		{
			// A tool may narrow the unit's capabilities and never widen
			// them, so the unit's list stays the whole account of what the
			// artifact can reach.
			name:    "tool capability the unit never declared",
			yaml:    strings.Replace(validWithTool, "capabilities: [storage]\ntools:", "capabilities: []\ntools:", 1),
			wantErr: "never widen them",
		},
		{
			name:    "unknown capability",
			yaml:    strings.Replace(validWithTool, "    capabilities: [storage]", "    capabilities: [gpu]", 1),
			wantErr: `unknown capability "gpu"`,
		},
		{
			name: "tools on a script unit",
			yaml: strings.Replace(validWithTool, "  type: chat", "  type: script\n  script: run.sh", 1),
			// A script unit runs its own program; the model is not in
			// charge, so there is nothing to offer tools to.
			wantErr: "only used by chat units",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("Parse() succeeded, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Unit-level capabilities were never validated, so a typo silently granted
// nothing while reading as though it granted something.
func TestUnitCapabilityValidation(t *testing.T) {
	_, err := Parse([]byte(valid + "capabilities: [netwrok]\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown capability "netwrok"`) {
		t.Errorf("error = %v, want a rejected typo", err)
	}
}
