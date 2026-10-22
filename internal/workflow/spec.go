// Package workflow implements Agent Compose: a declarative multi-agent
// workflow, described by one nexus-compose.yaml, that can be validated,
// executed, and packaged as a single OCI artifact.
//
// The existing sequential pipeline (package compose) answers "run these
// units in order". A workflow answers the harder question: which agents
// feed which, under what condition, reshaping the payload how, with what
// shared state — declared once, in a file that travels with the artifact.
//
// Everything here is checkable without running a model. A workflow whose
// graph has a cycle, whose route names an agent that does not exist, or
// whose condition does not parse, fails at `nexus compose validate`
// rather than three stages into a run that has already burned GPU time.
package workflow

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// FileName is the canonical workflow file name.
	FileName = "nexus-compose.yaml"

	// APIVersion and Kind are the schema identifiers. They share the
	// apiVersion of nexus.yaml deliberately: a workflow and a unit are two
	// kinds in one schema family, not two unrelated formats.
	APIVersion = "nexusrun.dev/v1"
	Kind       = "Workflow"
)

// State bus backends.
const (
	StateMemory = "memory"
	StateFile   = "file"
)

// Network isolation modes.
const (
	IsolationProcess   = "process"
	IsolationNamespace = "namespace"
	IsolationNone      = "none"
)

// EncryptionAESGCM is the only supported state encryption.
const EncryptionAESGCM = "aes256-gcm"

// CondAlways is the condition that routes unconditionally. It is spelled
// out rather than left implicit because a route with no condition and a
// route that always fires should not look different in a diff.
const CondAlways = "always"

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// agentNameRe is looser than nameRe: an agent name is also an identifier
// in conditions and templates, so underscores must be allowed, and it can
// never start with a digit.
var agentNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Spec is the parsed nexus-compose.yaml.
type Spec struct {
	APIVersion  string `yaml:"apiVersion" json:"apiVersion"`
	Kind        string `yaml:"kind" json:"kind"`
	Name        string `yaml:"name" json:"name"`
	Version     string `yaml:"version" json:"version"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Author      string `yaml:"author,omitempty" json:"author,omitempty"`
	License     string `yaml:"license,omitempty" json:"license,omitempty"`

	Agents      map[string]Agent `yaml:"agents" json:"agents"`
	Routing     []Route          `yaml:"routing,omitempty" json:"routing,omitempty"`
	SharedState State            `yaml:"shared_state,omitempty" json:"shared_state,omitempty"`
	Network     Network          `yaml:"network,omitempty" json:"network,omitempty"`
}

// Agent is one participant in the workflow.
type Agent struct {
	// Unit is a unit reference: a local ref (name:version), a registry
	// reference, or a directory.
	Unit string `yaml:"unit" json:"unit"`

	// Model overrides the unit's own model for this workflow. It is how
	// the same unit serves as a cheap and an expensive stage in one file.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`

	Hardware  Hardware          `yaml:"hardware,omitempty" json:"hardware,omitempty"`
	DependsOn []string          `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	Env       map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	MaxTokens int `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`

	// Restart is how many times a crashed agent is retried before the
	// workflow gives up on it. Zero means one attempt, no retry.
	Restart int `yaml:"restart,omitempty" json:"restart,omitempty"`

	// Resource ceilings, enforced by the runner where the platform can.
	MaxMemoryMB   int `yaml:"max_memory_mb,omitempty" json:"max_memory_mb,omitempty"`
	MaxCPUPercent int `yaml:"max_cpu_percent,omitempty" json:"max_cpu_percent,omitempty"`
}

// Hardware mirrors the unit-level preference list.
type Hardware struct {
	Prefer []string `yaml:"prefer,omitempty" json:"prefer,omitempty"`
}

// Route is one data-flow edge: what moves from one agent to another, when.
type Route struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`

	// Condition is an expression evaluated against the outputs produced so
	// far. Empty means CondAlways.
	Condition string `yaml:"condition,omitempty" json:"condition,omitempty"`

	// Transform is a text/template reshaping the payload before it is
	// delivered. Empty passes the sender's output through unchanged.
	Transform string `yaml:"transform,omitempty" json:"transform,omitempty"`
}

// State configures the shared message bus.
type State struct {
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`

	// Encryption, when set, encrypts every record with a key derived from
	// NEXUS_STATE_KEY. The bus holds whatever agents said to each other,
	// which on a shared machine is usually the most sensitive artifact a
	// workflow leaves behind.
	Encryption string `yaml:"encryption,omitempty" json:"encryption,omitempty"`
}

// Network controls how far an agent's process can reach.
type Network struct {
	Isolation     string `yaml:"isolation,omitempty" json:"isolation,omitempty"`
	AllowLoopback bool   `yaml:"allow_loopback,omitempty" json:"allow_loopback,omitempty"`
}

// Parse decodes and validates workflow bytes.
func Parse(data []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	s.applyDefaults()
	return &s, nil
}

func (s *Spec) applyDefaults() {
	if s.SharedState.Backend == "" {
		s.SharedState.Backend = StateMemory
	}
	if s.SharedState.Backend == StateFile && s.SharedState.Path == "" {
		s.SharedState.Path = "./.nexus/state.jsonl"
	}
	if s.Network.Isolation == "" {
		s.Network.Isolation = IsolationProcess
	}
	for i := range s.Routing {
		if s.Routing[i].Condition == "" {
			s.Routing[i].Condition = CondAlways
		}
	}
}

// Ref returns the canonical name:version reference for the workflow.
func (s *Spec) Ref() string { return s.Name + ":" + s.Version }

// AgentNames returns agent names in a stable order. Map iteration order is
// random, and a workflow whose validation errors, logs, and packed config
// come out shuffled between runs is one nobody can diff.
func (s *Spec) AgentNames() []string {
	names := make([]string, 0, len(s.Agents))
	for n := range s.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Diagnostic is one validation problem, tied to the place in the file it
// concerns so a tool can point at it. Message is the same text Validate prints.
type Diagnostic struct {
	// Path locates the problem in the file: "agents.writer.unit",
	// "routing[1].condition", "shared_state.backend". Empty means the whole
	// file.
	Path    string `json:"path"`
	Message string `json:"message"`
	// Agents names the agents involved, when the problem is about specific
	// ones (a dependency cycle, a route between two agents).
	Agents []string `json:"agents,omitempty"`
	// Line is the 1-based line, set only for YAML syntax problems, where no
	// path exists yet.
	Line int `json:"line,omitempty"`
}

type diagnostics []Diagnostic

func (d *diagnostics) add(path string, agents []string, format string, args ...any) {
	*d = append(*d, Diagnostic{Path: path, Agents: agents, Message: fmt.Sprintf(format, args...)})
}

// Validate checks everything checkable without executing anything.
func (s *Spec) Validate() error {
	ds := s.Diagnostics()
	if len(ds) == 0 {
		return nil
	}
	msgs := make([]string, len(ds))
	for i, d := range ds {
		msgs[i] = d.Message
	}
	return fmt.Errorf("invalid %s:\n  - %s", FileName, strings.Join(msgs, "\n  - "))
}

// Diagnostics is Validate with each problem located. An empty result means the
// workflow is valid.
func (s *Spec) Diagnostics() []Diagnostic {
	var ds diagnostics

	if s.APIVersion != APIVersion {
		ds.add("apiVersion", nil, "apiVersion must be %q, got %q", APIVersion, s.APIVersion)
	}
	if s.Kind != Kind {
		ds.add("kind", nil, "kind must be %q, got %q — a unit file is kind Unit and belongs in %s",
			Kind, s.Kind, "nexus.yaml")
	}
	if !nameRe.MatchString(s.Name) {
		ds.add("name", nil, "name %q must be lowercase alphanumeric with hyphens", s.Name)
	}
	if s.Version == "" {
		ds.add("version", nil, "version is required")
	}
	if len(s.Agents) == 0 {
		ds.add("agents", nil, "at least one agent is required")
	}

	for _, name := range s.AgentNames() {
		a := s.Agents[name]
		label := fmt.Sprintf("agents.%s", name)
		who := []string{name}
		if !agentNameRe.MatchString(name) {
			ds.add(label, who,
				"%s: name must start with a letter or underscore and contain only letters, digits and underscores — it is also an identifier in conditions and transforms",
				label)
		}
		if strings.TrimSpace(a.Unit) == "" {
			ds.add(label+".unit", who, "%s: unit is required", label)
		}
		for _, p := range a.Hardware.Prefer {
			if p != "npu" && p != "gpu" && p != "cpu" {
				ds.add(label+".hardware.prefer", who, "%s: hardware.prefer: unknown accelerator %q", label, p)
			}
		}
		for _, d := range a.DependsOn {
			if _, ok := s.Agents[d]; !ok {
				ds.add(label+".depends_on", who, "%s: depends_on names %q, which is not an agent in this workflow", label, d)
			}
			if d == name {
				ds.add(label+".depends_on", who, "%s: depends_on itself", label)
			}
		}
		if a.MaxTokens < 0 {
			ds.add(label+".max_tokens", who, "%s: max_tokens cannot be negative", label)
		}
		if a.Restart < 0 {
			ds.add(label+".restart", who, "%s: restart cannot be negative", label)
		}
	}

	ds = append(ds, s.validateRouting()...)
	ds = append(ds, s.validateState()...)

	switch s.Network.Isolation {
	case "", IsolationProcess, IsolationNamespace, IsolationNone:
	default:
		ds.add("network.isolation", nil, "network.isolation must be %s, %s or %s, got %q",
			IsolationProcess, IsolationNamespace, IsolationNone, s.Network.Isolation)
	}

	// Cycle detection runs last: it needs the edge set to be sane, and a
	// cycle report naming agents that do not exist is worse than useless.
	if len(ds) == 0 {
		if _, err := s.Order(); err != nil {
			var cyc *CycleError
			if errors.As(err, &cyc) {
				ds.add("agents", cyc.Agents, "%s", err.Error())
			} else {
				ds.add("", nil, "%s", err.Error())
			}
		}
	}
	return ds
}

func (s *Spec) validateRouting() diagnostics {
	var ds diagnostics
	seen := map[string]bool{}
	for i, r := range s.Routing {
		at := fmt.Sprintf("routing[%d]", i)
		label := at
		var who []string
		if r.From != "" && r.To != "" {
			label = fmt.Sprintf("routing[%d] (%s → %s)", i, r.From, r.To)
			who = []string{r.From, r.To}
		}
		if r.From == "" {
			ds.add(at+".from", who, "%s: from is required", label)
		} else if _, ok := s.Agents[r.From]; !ok {
			ds.add(at+".from", who, "%s: from names %q, which is not an agent in this workflow", label, r.From)
		}
		if r.To == "" {
			ds.add(at+".to", who, "%s: to is required", label)
		} else if _, ok := s.Agents[r.To]; !ok {
			ds.add(at+".to", who, "%s: to names %q, which is not an agent in this workflow", label, r.To)
		}
		if r.From != "" && r.From == r.To {
			ds.add(at, who, "%s: an agent cannot route to itself", label)
		}

		// Two routes on one edge would deliver the sender's output twice,
		// which is never what was meant and is invisible in the output.
		key := r.From + "\x00" + r.To
		if r.From != "" && r.To != "" {
			if seen[key] {
				ds.add(at, who, "%s: duplicate route — this edge is already declared", label)
			}
			seen[key] = true
		}

		cond := r.Condition
		if cond == "" {
			cond = CondAlways
		}
		if err := CheckCondition(cond, s.AgentNames()); err != nil {
			ds.add(at+".condition", who, "%s: condition: %v", label, err)
		}
		if r.Transform != "" {
			if err := CheckTransform(r.Transform); err != nil {
				ds.add(at+".transform", who, "%s: transform: %v", label, err)
			}
		}
	}
	return ds
}

func (s *Spec) validateState() diagnostics {
	var ds diagnostics
	switch s.SharedState.Backend {
	case "", StateMemory, StateFile:
	case "sqlite":
		// SQLite is the obvious default. Every pure-Go SQLite
		// driver is larger than this entire binary, and the cgo ones break
		// static cross-compilation to the 32-bit ARM targets NexusRun
		// exists to reach — so the file backend provides what SQLite was
		// wanted for (durable, single-file, survives a crash) at no size cost.
		ds.add("shared_state.backend", nil,
			"shared_state.backend: sqlite is not supported — it would cost more binary than the whole runtime. Use %q for a durable single-file bus, or %q for an ephemeral one",
			StateFile, StateMemory)
	default:
		ds.add("shared_state.backend", nil, "shared_state.backend must be %s or %s, got %q",
			StateMemory, StateFile, s.SharedState.Backend)
	}
	if s.SharedState.Backend == StateMemory && s.SharedState.Path != "" {
		ds.add("shared_state.path", nil, "shared_state.path is set but the memory backend never writes a file")
	}
	// The bus appends to this file, and a workflow pulled from a registry is
	// someone else's file: an absolute or escaping path would let it create
	// or append to any file the user can write.
	if p := s.SharedState.Path; p != "" {
		clean := filepath.Clean(filepath.FromSlash(p))
		if filepath.IsAbs(clean) || strings.HasPrefix(p, "/") || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			ds.add("shared_state.path", nil, "shared_state.path must be a relative path inside the workflow's folder, got %q", p)
		}
	}
	switch s.SharedState.Encryption {
	case "", EncryptionAESGCM:
	default:
		ds.add("shared_state.encryption", nil, "shared_state.encryption must be %q, got %q",
			EncryptionAESGCM, s.SharedState.Encryption)
	}
	if s.SharedState.Encryption != "" && s.SharedState.Backend != StateFile {
		ds.add("shared_state.encryption", nil, "shared_state.encryption applies to the file backend; the memory bus never leaves the process")
	}
	return ds
}

// Marshal serializes the workflow back to YAML.
func (s *Spec) Marshal() ([]byte, error) { return yaml.Marshal(s) }
