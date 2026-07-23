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
	Tools        []string   `yaml:"tools,omitempty"`
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
	if len(errs) > 0 {
		return fmt.Errorf("invalid manifest:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Ref returns the canonical name:version reference for the unit.
func (m *Manifest) Ref() string { return m.Name + ":" + m.Version }

// Marshal serializes the manifest back to YAML.
func (m *Manifest) Marshal() ([]byte, error) { return yaml.Marshal(m) }
