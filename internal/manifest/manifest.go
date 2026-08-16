// Package manifest defines the nexus.yaml schema — the declarative
// description of a Nexus Unit: its models, entrypoint, capabilities,
// and hardware preferences.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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
//
// A model entry says either *which* model to use (Source) or *what kind*
// of model is needed (Requirements plus Candidates). The second form is
// how one artifact ships to fifty machines with different accelerators
// and picks the right weights on each: the author states the bar the
// model must clear, and the target machine measures which candidate
// clears it. See docs/AUTOMODEL.md.
type Model struct {
	ID     string `yaml:"id"`
	Source string `yaml:"source,omitempty"` // https://…, hf:org/repo/file.gguf, ollama:name:tag, or local path
	SHA256 string `yaml:"sha256,omitempty"` // optional integrity pin
	Format string `yaml:"format,omitempty"` // gguf (default), onnx

	Context     int      `yaml:"context,omitempty"`     // context window tokens
	Temperature *float64 `yaml:"temperature,omitempty"` // sampling temperature

	// Profile names an auto-selection profile. It is documentation for the
	// reader and the key benchmark results are cached under.
	Profile string `yaml:"profile,omitempty"`

	Requirements *Requirements `yaml:"requirements,omitempty"`
	Candidates   []Candidate   `yaml:"candidates,omitempty"`

	// SelectionStrategy decides between candidates that all clear the bar.
	SelectionStrategy string `yaml:"selection_strategy,omitempty"`
}

// Auto-selection strategies.
const (
	// StrategyBestEval takes the highest eval score, tie-broken by speed.
	StrategyBestEval = "best_eval_score"
	// StrategyFastestPassing takes the fastest model that clears the bar,
	// which is what an interactive agent on a kiosk actually wants.
	StrategyFastestPassing = "fastest_passing"
	// StrategySmallestPassing takes the smallest model that clears the bar,
	// for devices where resident memory is the binding constraint.
	StrategySmallestPassing = "smallest_passing"
)

// Tool-calling requirement levels.
const (
	ToolCallingRequired  = "required"
	ToolCallingPreferred = "preferred"
	ToolCallingNone      = "none"
)

// Requirements is the bar a candidate model must clear.
type Requirements struct {
	MinContext  int    `yaml:"min_context,omitempty"`
	ToolCalling string `yaml:"tool_calling,omitempty"`
	MaxSizeMB   int64  `yaml:"max_size_mb,omitempty"`

	// MinQualityScore is the number of eval cases that must pass, out of
	// the unit's own suite — the roadmap's "11 out of 13". It is a count
	// rather than a percentage because that is the number a suite author
	// actually reasons about, and the suite travels with the unit, so the
	// denominator is not ambiguous.
	MinQualityScore int `yaml:"min_quality_score,omitempty"`
}

// Candidate is one model auto-selection may choose.
type Candidate struct {
	Source string `yaml:"source"`

	// Weight is an author's prior, applied to the eval score as a
	// tiebreaker preference. It expresses "these are equivalent on paper,
	// but I trust this one" without hardcoding the choice.
	Weight float64 `yaml:"weight,omitempty"`

	// Context overrides the profile's context window for this candidate,
	// for models whose window differs from the others'.
	Context int `yaml:"context,omitempty"`

	SHA256 string `yaml:"sha256,omitempty"`
}

// Auto reports whether this entry selects a model rather than naming one.
func (m Model) Auto() bool { return len(m.Candidates) > 0 }

// Strategy returns the effective selection strategy.
func (m Model) Strategy() string {
	if m.SelectionStrategy == "" {
		return StrategyBestEval
	}
	return m.SelectionStrategy
}

// ProfileName returns the profile key this entry caches results under.
func (m Model) ProfileName() string {
	if m.Profile != "" {
		return m.Profile
	}
	return "default"
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

// SecretRef declares that the unit needs a secret, without carrying it.
//
// This is the whole point of the split: the artifact is public and says
// which credentials it requires; the values live in the local encrypted
// store on whichever machine runs it. `nexus inspect` shows the names so a
// reader knows what an agent will reach for, and never a value or even
// whether one is present.
type SecretRef struct {
	Name     string `yaml:"name"`
	Required bool   `yaml:"required,omitempty"`

	// Env overrides the environment variable the value is injected as.
	// Defaults to Name.
	Env string `yaml:"env,omitempty"`

	// MountPath writes the value to a file instead, for secrets that are
	// certificates or key files rather than tokens. The file lives in a
	// per-run temporary directory and is removed when the run ends.
	MountPath string `yaml:"mount_path,omitempty"`
}

// ConfigRef is a non-secret setting with a default, injected the same way.
// It exists so the two do not get conflated: a value with a default in the
// manifest is by definition not a secret.
type ConfigRef struct {
	Name    string `yaml:"name"`
	Default string `yaml:"default,omitempty"`
	Env     string `yaml:"env,omitempty"`
}

// EnvName returns the environment variable a secret is injected as.
func (s SecretRef) EnvName() string {
	if s.Env != "" {
		return s.Env
	}
	return s.Name
}

// EnvName returns the environment variable a config value is injected as.
func (c ConfigRef) EnvName() string {
	if c.Env != "" {
		return c.Env
	}
	return c.Name
}

// MCPServer is an external tool server the unit depends on.
//
// Declaring it here is what makes the dependency travel with the artifact:
// the source is version-pinned like a model, fetched into a shared local
// cache, and started under the same confinement as everything else. The
// usual arrangement — the operator installs and runs servers, and the agent
// trusts whatever is listening — is what a portable unit exists to avoid.
type MCPServer struct {
	// Source is github:org/repo#ref/path, file:///abs/path, npm:package,
	// exec:command, or an OCI reference.
	Source string `yaml:"source"`

	// Command overrides how the server is launched. Most servers need it:
	// a cloned repository is not self-describing about which file to run.
	Command []string `yaml:"command,omitempty"`

	Env map[string]string `yaml:"env,omitempty"`

	Sandbox MCPSandbox `yaml:"sandbox,omitempty"`

	// Tools, when set, narrows which of the server's tools are offered to
	// the model. A filesystem server exposing eleven tools when the unit
	// needs two is eleven ways for the model to go wrong.
	Tools []string `yaml:"tools,omitempty"`

	// Timeout bounds a single tool call. Default 30s.
	Timeout string `yaml:"timeout,omitempty"`
}

// MCPSandbox narrows what a server process may touch.
type MCPSandbox struct {
	ReadOnly     bool     `yaml:"read_only,omitempty"`
	AllowedPaths []string `yaml:"allowed_paths,omitempty"`
	BlockedPaths []string `yaml:"blocked_paths,omitempty"`
	Network      bool     `yaml:"network,omitempty"`
}

// Manifest is the parsed nexus.yaml.
type Manifest struct {
	APIVersion   string               `yaml:"apiVersion"`
	Name         string               `yaml:"name"`
	Version      string               `yaml:"version"`
	Description  string               `yaml:"description,omitempty"`
	Author       string               `yaml:"author,omitempty"`
	License      string               `yaml:"license,omitempty"`
	Models       []Model              `yaml:"models"`
	Entrypoint   Entrypoint           `yaml:"entrypoint"`
	Tools        []Tool               `yaml:"tools,omitempty"`
	MCPServers   map[string]MCPServer `yaml:"mcp_servers,omitempty"`
	Secrets      []SecretRef          `yaml:"secrets,omitempty"`
	Config       []ConfigRef          `yaml:"config,omitempty"`
	Capabilities []string             `yaml:"capabilities,omitempty"`
	Hardware     Hardware             `yaml:"hardware,omitempty"`
}

// MCPNames returns server names in a stable order.
func (m *Manifest) MCPNames() []string {
	names := make([]string, 0, len(m.MCPServers))
	for n := range m.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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
	errs = append(errs, m.validateModels()...)
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
	errs = append(errs, m.validateSecrets()...)
	errs = append(errs, m.validateMCP()...)
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

// validateModels checks each model entry is one of the two valid shapes:
// a named model, or an auto-selection profile.
func (m *Manifest) validateModels() []string {
	var errs []string
	seenID := map[string]bool{}
	for i, mod := range m.Models {
		label := fmt.Sprintf("models[%d]", i)
		if mod.ID != "" {
			label = fmt.Sprintf("models[%d] (%s)", i, mod.ID)
		}

		switch {
		case mod.Source == "" && !mod.Auto():
			errs = append(errs, label+": source is required, or declare candidates to have one selected on the target machine")
		case mod.Source != "" && mod.Auto():
			// Both would leave it genuinely unclear which the author meant,
			// and picking one silently is the wrong kind of helpful.
			errs = append(errs, label+": has both source and candidates — a model entry either names a model or selects one, not both")
		}

		if mod.ID != "" {
			if seenID[mod.ID] {
				errs = append(errs, label+": duplicate model id")
			}
			seenID[mod.ID] = true
		}

		if !mod.Auto() {
			if mod.Requirements != nil {
				errs = append(errs, label+": requirements only apply to a model entry with candidates")
			}
			if mod.SelectionStrategy != "" {
				errs = append(errs, label+": selection_strategy only applies to a model entry with candidates")
			}
			continue
		}
		errs = append(errs, validateAutoModel(label, mod)...)
	}
	return errs
}

func validateAutoModel(label string, mod Model) []string {
	var errs []string

	switch mod.SelectionStrategy {
	case "", StrategyBestEval, StrategyFastestPassing, StrategySmallestPassing:
	default:
		errs = append(errs, fmt.Sprintf("%s: selection_strategy must be %s, %s or %s, got %q",
			label, StrategyBestEval, StrategyFastestPassing, StrategySmallestPassing, mod.SelectionStrategy))
	}

	seen := map[string]bool{}
	for j, c := range mod.Candidates {
		clabel := fmt.Sprintf("%s: candidates[%d]", label, j)
		if strings.TrimSpace(c.Source) == "" {
			errs = append(errs, clabel+": source is required")
			continue
		}
		if seen[c.Source] {
			// A duplicate would be benchmarked twice and could beat itself.
			errs = append(errs, clabel+": duplicate candidate source "+c.Source)
		}
		seen[c.Source] = true
		if c.Weight < 0 {
			errs = append(errs, clabel+": weight cannot be negative")
		}
		if c.Context < 0 {
			errs = append(errs, clabel+": context cannot be negative")
		}
	}

	if r := mod.Requirements; r != nil {
		switch r.ToolCalling {
		case "", ToolCallingRequired, ToolCallingPreferred, ToolCallingNone:
		default:
			errs = append(errs, fmt.Sprintf("%s: requirements.tool_calling must be %s, %s or %s, got %q",
				label, ToolCallingRequired, ToolCallingPreferred, ToolCallingNone, r.ToolCalling))
		}
		if r.MinContext < 0 {
			errs = append(errs, label+": requirements.min_context cannot be negative")
		}
		if r.MaxSizeMB < 0 {
			errs = append(errs, label+": requirements.max_size_mb cannot be negative")
		}
		if r.MinQualityScore < 0 {
			errs = append(errs, label+": requirements.min_quality_score cannot be negative")
		}
	}

	// A strategy that ranks by "passing" has no meaning without a bar to
	// pass: every candidate would qualify, and the choice would collapse
	// to whichever happened to be smallest or fastest regardless of whether
	// it can do the job at all.
	strat := mod.Strategy()
	if strat == StrategyFastestPassing || strat == StrategySmallestPassing {
		if mod.Requirements == nil || mod.Requirements.MinQualityScore == 0 {
			errs = append(errs, fmt.Sprintf(
				"%s: selection_strategy %s needs requirements.min_quality_score — without a bar, every candidate passes and the choice ignores quality entirely",
				label, strat))
		}
	}
	return errs
}

// mcpNameRe is the server-name syntax. The name prefixes every tool the
// server contributes, so it has to survive being part of a tool name.
var mcpNameRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,31}$`)

// validateMCP checks the declared tool servers.
func (m *Manifest) validateMCP() []string {
	var errs []string
	if len(m.MCPServers) > 0 && m.Entrypoint.Type == "script" {
		// A script unit runs its own program; nothing is offering tools to
		// a model, so the servers would be started and never consulted.
		errs = append(errs, "mcp_servers are used by chat units; a script unit calls whatever it likes directly")
	}

	for _, name := range m.MCPNames() {
		srv := m.MCPServers[name]
		label := "mcp_servers." + name
		if !mcpNameRe.MatchString(name) {
			errs = append(errs, fmt.Sprintf(
				"%s: name must be 1–32 characters of letters, digits, underscore or hyphen — it prefixes every tool this server offers",
				label))
		}
		if strings.TrimSpace(srv.Source) == "" {
			errs = append(errs, label+": source is required")
		}
		if srv.Timeout != "" {
			if _, err := time.ParseDuration(srv.Timeout); err != nil {
				errs = append(errs, fmt.Sprintf("%s: timeout %q is not a duration (e.g. 30s)", label, srv.Timeout))
			}
		}
		for _, p := range srv.Sandbox.AllowedPaths {
			if !filepath.IsAbs(p) {
				errs = append(errs, fmt.Sprintf("%s: sandbox.allowed_paths %q must be absolute", label, p))
			}
		}
		for _, p := range srv.Sandbox.BlockedPaths {
			if !filepath.IsAbs(p) {
				errs = append(errs, fmt.Sprintf("%s: sandbox.blocked_paths %q must be absolute", label, p))
			}
		}
		// A server granted network access by its own sandbox block, in a
		// unit that never declared the capability, would widen what the
		// artifact can reach beyond what its top-level list admits to.
		if srv.Sandbox.Network && !containsStr(m.Capabilities, "network") {
			errs = append(errs, fmt.Sprintf(
				"%s: sandbox.network is set but the unit does not declare the network capability — a server cannot widen what the unit admits to reaching",
				label))
		}
	}
	return errs
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// envNameRe is the shape an injected name must have. A declaration that
// cannot become an environment variable would be accepted and then never
// reach the agent, which looks like a missing secret rather than a typo.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateSecrets checks the secret and config declarations. The two share
// one environment namespace, so a collision between them is an error here
// rather than a coin flip at run time.
func (m *Manifest) validateSecrets() []string {
	var errs []string
	claimed := map[string]string{} // env var → what declared it

	for i, s := range m.Secrets {
		label := fmt.Sprintf("secrets[%d]", i)
		if s.Name != "" {
			label = fmt.Sprintf("secrets[%d] (%s)", i, s.Name)
		}
		if s.Name == "" {
			errs = append(errs, label+": name is required")
			continue
		}
		if !envNameRe.MatchString(s.Name) {
			errs = append(errs, fmt.Sprintf("%s: name must be a valid environment variable name", label))
		}
		if s.Env != "" && !envNameRe.MatchString(s.Env) {
			errs = append(errs, fmt.Sprintf("%s: env %q is not a valid environment variable name", label, s.Env))
		}
		if s.MountPath != "" && !filepath.IsAbs(s.MountPath) {
			// A relative mount path would resolve against whatever the
			// working directory happened to be.
			errs = append(errs, fmt.Sprintf("%s: mount_path must be absolute", label))
		}
		if prev, dup := claimed[s.EnvName()]; dup {
			errs = append(errs, fmt.Sprintf("%s: %s is already injected by %s", label, s.EnvName(), prev))
		}
		claimed[s.EnvName()] = label
	}

	for i, c := range m.Config {
		label := fmt.Sprintf("config[%d]", i)
		if c.Name != "" {
			label = fmt.Sprintf("config[%d] (%s)", i, c.Name)
		}
		if c.Name == "" {
			errs = append(errs, label+": name is required")
			continue
		}
		if !envNameRe.MatchString(c.Name) {
			errs = append(errs, fmt.Sprintf("%s: name must be a valid environment variable name", label))
		}
		if c.Env != "" && !envNameRe.MatchString(c.Env) {
			errs = append(errs, fmt.Sprintf("%s: env %q is not a valid environment variable name", label, c.Env))
		}
		if prev, dup := claimed[c.EnvName()]; dup {
			errs = append(errs, fmt.Sprintf("%s: %s is already injected by %s", label, c.EnvName(), prev))
		}
		claimed[c.EnvName()] = label
	}
	return errs
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
