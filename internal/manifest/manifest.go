// Package manifest defines the nexus.yaml schema — the declarative
// description of a Nexus Unit: its models, entrypoint, capabilities,
// and hardware preferences.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// FileName is the canonical manifest file name inside a unit directory.
	FileName = "nexus.yaml"

	// APIVersion is the current schema version.
	APIVersion = "nexusrun.dev/v1"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Model describes one model the unit depends on. Models are referenced,
// not embedded: the runtime resolves Source into a shared local cache so
// units stay small and models are deduplicated across units.
type Model struct {
	ID     string `yaml:"id"`
	Source string `yaml:"source"`           // https://…, hf:org/repo/file.gguf, ollama:name:tag, or local path
	SHA256 string `yaml:"sha256,omitempty"` // optional integrity pin
	Format string `yaml:"format,omitempty"` // gguf (default), onnx

	Context     int      `yaml:"context,omitempty"`     // context window tokens
	Temperature *float64 `yaml:"temperature,omitempty"` // sampling temperature
}

// Entrypoint describes how the unit runs.
type Entrypoint struct {
	Type         string `yaml:"type"` // "chat" (built-in) or "script"
	Script       string `yaml:"script,omitempty"`
	SystemPrompt string `yaml:"system_prompt,omitempty"`
}

// Hardware expresses the unit's execution preferences. The runtime
// intersects this with what it detects on the host.
type Hardware struct {
	// Prefer is an ordered list of accelerator classes: npu, gpu, cpu.
	// Default: [npu, gpu, cpu] — accelerate when possible.
	Prefer      []string `yaml:"prefer,omitempty"`
	MinMemoryGB float64  `yaml:"min_memory_gb,omitempty"`
}

// Tool is one tool the unit's model may invoke.
//
// A tool is what makes a unit an agent rather than a prompt, and it is the
// reason the artifact format earns its keep: the model, the instructions,
// the tool schemas, and the permissions each tool needs are one versioned,
// digest-addressed thing instead of four paragraphs of README.
type Tool struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Parameters is a JSON Schema object describing the arguments. It is
	// passed to the model untouched, so anything a backend understands can
	// be expressed here. Omit it for a tool that takes no arguments.
	Parameters map[string]any `yaml:"parameters,omitempty"`

	Exec ToolExec `yaml:"exec"`

	// Capabilities narrows what this tool may do, and can only ever
	// narrow: it must be a subset of the unit's own declared capabilities.
	// A unit's top-level list therefore stays the complete account of what
	// the artifact can reach, which is the property that makes it worth
	// reading before running someone else's unit.
	Capabilities []string `yaml:"capabilities,omitempty"`
}

// ToolExec says how a tool call is carried out.
type ToolExec struct {
	// Type is "script" — a program in the unit, handed the call's
	// arguments as JSON on stdin and answering with JSON on stdout.
	Type string `yaml:"type"`

	// Command is argv, resolved relative to the unit directory.
	Command []string `yaml:"command,omitempty"`
}

// Tool exec types.
const ToolExecScript = "script"

// Recognised capability names, shared with the sandbox policy.
var knownCapabilities = map[string]bool{"network": true, "storage": true}

// toolNameRe is the name syntax every tool-calling backend accepts.
var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Manifest is the parsed nexus.yaml.
type Manifest struct {
	APIVersion   string     `yaml:"apiVersion"`
	Name         string     `yaml:"name"`
	Version      string     `yaml:"version"`
	Description  string     `yaml:"description,omitempty"`
	Author       string     `yaml:"author,omitempty"`
	License      string     `yaml:"license,omitempty"`
	Models       []Model    `yaml:"models"`
	Entrypoint   Entrypoint `yaml:"entrypoint"`
	Tools        []Tool     `yaml:"tools,omitempty"`
	Capabilities []string   `yaml:"capabilities,omitempty"`
	Hardware     Hardware   `yaml:"hardware,omitempty"`
}

// Load reads and validates a manifest from a unit directory or file path.
func Load(path string) (*Manifest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		path = filepath.Join(path, FileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates manifest bytes.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse nexus.yaml: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.applyDefaults()
	return &m, nil
}

func (m *Manifest) applyDefaults() {
	if len(m.Hardware.Prefer) == 0 {
		m.Hardware.Prefer = []string{"npu", "gpu", "cpu"}
	}
	for i := range m.Models {
		if m.Models[i].Format == "" {
			m.Models[i].Format = "gguf"
		}
		if m.Models[i].ID == "" {
			m.Models[i].ID = "main"
		}
	}
	if m.Entrypoint.Type == "" {
		m.Entrypoint.Type = "chat"
	}
}

// Validate checks structural invariants.
func (m *Manifest) Validate() error {
	var errs []string
	if m.APIVersion != APIVersion {
		errs = append(errs, fmt.Sprintf("apiVersion must be %q, got %q", APIVersion, m.APIVersion))
	}
	if !nameRe.MatchString(m.Name) {
		errs = append(errs, fmt.Sprintf("name %q must be lowercase alphanumeric with hyphens", m.Name))
	}
	if m.Version == "" {
		errs = append(errs, "version is required")
	}
	if len(m.Models) == 0 && m.Entrypoint.Type != "script" {
		errs = append(errs, "at least one model is required for chat units")
	}
	for i, mod := range m.Models {
		if mod.Source == "" {
			errs = append(errs, fmt.Sprintf("models[%d]: source is required", i))
		}
	}
	if t := m.Entrypoint.Type; t != "" && t != "chat" && t != "script" {
		errs = append(errs, fmt.Sprintf("entrypoint.type must be chat or script, got %q", t))
	}
	for _, p := range m.Hardware.Prefer {
		if p != "npu" && p != "gpu" && p != "cpu" {
			errs = append(errs, fmt.Sprintf("hardware.prefer: unknown accelerator %q", p))
		}
	}
	for i, c := range m.Capabilities {
		if !knownCapabilities[c] {
			errs = append(errs, fmt.Sprintf("capabilities[%d]: unknown capability %q", i, c))
		}
	}
	errs = append(errs, m.validateTools()...)
	if len(m.Tools) > 0 && m.Entrypoint.Type == "script" {
		// A script unit runs its own program; the model is not in charge,
		// so there is nothing to offer tools to.
		errs = append(errs, "tools are only used by chat units; a script unit calls whatever it likes directly")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid manifest:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateTools checks the tool declarations. Everything here is checkable
// without running anything, and a tool that is wrong in these ways cannot
// work at all: the model would be offered something it cannot address, or
// the runtime asked to execute nothing.
func (m *Manifest) validateTools() []string {
	var errs []string
	declared := map[string]bool{}
	for _, c := range m.Capabilities {
		declared[c] = true
	}
	seen := map[string]bool{}
	for i, t := range m.Tools {
		label := fmt.Sprintf("tools[%d]", i)
		if t.Name != "" {
			label = fmt.Sprintf("tools[%d] (%s)", i, t.Name)
		}
		switch {
		case t.Name == "":
			errs = append(errs, label+": name is required")
		case !toolNameRe.MatchString(t.Name):
			errs = append(errs, fmt.Sprintf("%s: name must be 1–64 characters of letters, digits, underscore or hyphen", label))
		case seen[t.Name]:
			// The model addresses tools by name; two tools sharing one
			// leaves no way to say which was called.
			errs = append(errs, label+": duplicate tool name")
		}
		seen[t.Name] = true

		// The description is how the model decides whether to call the
		// tool. Without one it is nearly always ignored, which looks like
		// a broken runtime rather than a missing sentence.
		if strings.TrimSpace(t.Description) == "" {
			errs = append(errs, label+": description is required — it is what the model selects the tool by")
		}
		if t.Parameters != nil {
			if typ, ok := t.Parameters["type"].(string); !ok || typ != "object" {
				errs = append(errs, label+`: parameters must be a JSON Schema object (type: object)`)
			}
		}
		switch t.Exec.Type {
		case "":
			errs = append(errs, label+": exec.type is required")
		case ToolExecScript:
			if len(t.Exec.Command) == 0 {
				errs = append(errs, label+": exec.command is required for a script tool")
			}
		default:
			errs = append(errs, fmt.Sprintf("%s: exec.type must be %q, got %q", label, ToolExecScript, t.Exec.Type))
		}
		for _, c := range t.Capabilities {
			if !knownCapabilities[c] {
				errs = append(errs, fmt.Sprintf("%s: unknown capability %q", label, c))
				continue
			}
			if !declared[c] {
				errs = append(errs, fmt.Sprintf(
					"%s: capability %q is not declared by the unit — a tool can narrow the unit's capabilities, never widen them",
					label, c))
			}
		}
	}
	return errs
}

// Ref returns the canonical name:version reference for the unit.
func (m *Manifest) Ref() string { return m.Name + ":" + m.Version }

// Marshal serializes the manifest back to YAML.
func (m *Manifest) Marshal() ([]byte, error) { return yaml.Marshal(m) }
