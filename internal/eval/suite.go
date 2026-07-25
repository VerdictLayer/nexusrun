// Package eval scores a unit against a suite of cases, and records the
// score against the exact conditions that produced it.
//
// The condition part is the point. Existing eval tooling assumes a cloud
// API endpoint, where the only variable is the prompt. Local agents have
// three more: which quantization of the weights, which backend build, and
// which device. Those change the answers, not just the speed — a unit that
// passes at q8 on a workstation can fail at q3 on a Pi, and nothing tells
// you until a user reports nonsense.
//
// So a result here is never just a percentage. It is a percentage keyed to
// (unit digest, model digest, backend, device, host), which is the only
// form of the number that means anything when the same unit ships to
// mixed hardware.
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// APIVersion is the suite schema version, shared with nexus.yaml.
	APIVersion = "nexusrun.dev/v1"

	// Kind identifies an eval suite document, so a suite file and a unit
	// manifest cannot be confused for one another.
	Kind = "EvalSuite"

	// DirName is the conventional location for suites inside a unit.
	// Anything in the unit directory is packed into the artifact's source
	// layer, so suites travel with the unit: whoever pulls it can rerun
	// the evaluation themselves rather than trusting a published claim.
	DirName = "evals"
)

// Suite is a set of cases plus the sampling settings they assume.
type Suite struct {
	APIVersion  string `yaml:"apiVersion"`
	Kind        string `yaml:"kind"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`

	// Temperature defaults to 0. Sampling noise turns a suite into a coin
	// flip: cases pass on Tuesday and fail on Wednesday with nothing
	// changed, which trains people to ignore the result. Raise it only to
	// deliberately measure how stable a unit is under sampling, and pair
	// it with repeats.
	Temperature *float64 `yaml:"temperature,omitempty"`

	// MaxTokens caps generation per case. Low is good: most assertions
	// look at the first line, and a runaway generation multiplies suite
	// runtime by the number of cases and repeats.
	MaxTokens int `yaml:"max_tokens,omitempty"`

	// CaseSensitive makes string assertions exact. It defaults to false
	// because models vary casing freely ("Timeout" vs "timeout") and a
	// suite that fails on that is measuring nothing useful.
	CaseSensitive bool `yaml:"case_sensitive,omitempty"`

	// System overrides the unit's own system prompt for every case. Use
	// it to evaluate a candidate prompt without editing the unit.
	System string `yaml:"system,omitempty"`

	Cases []Case `yaml:"cases"`
}

// Case is one prompt and what the output must look like.
type Case struct {
	Name      string `yaml:"name"`
	Prompt    string `yaml:"prompt"`
	System    string `yaml:"system,omitempty"`
	MaxTokens int    `yaml:"max_tokens,omitempty"`
	Expect    Expect `yaml:"expect"`
}

// Parse decodes and validates suite bytes.
func Parse(data []byte) (*Suite, error) {
	var s Suite
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// Unknown fields are an error for the same reason they are in
	// nexus.yaml: a mistyped assertion key would otherwise silently
	// become a case with nothing to check, which passes.
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse eval suite: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Load reads a suite from a file path.
func Load(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Validate checks the suite is runnable. Everything checkable before
// execution is checked here, because a suite is slow: discovering a
// mistyped regex after twenty minutes of generation is the worst possible
// time to discover it.
func (s *Suite) Validate() error {
	var errs []string
	if s.APIVersion != APIVersion {
		errs = append(errs, fmt.Sprintf("apiVersion must be %q, got %q", APIVersion, s.APIVersion))
	}
	if s.Kind != Kind {
		errs = append(errs, fmt.Sprintf("kind must be %q, got %q", Kind, s.Kind))
	}
	if strings.TrimSpace(s.Name) == "" {
		errs = append(errs, "name is required")
	}
	if len(s.Cases) == 0 {
		errs = append(errs, "at least one case is required")
	}
	if s.Temperature != nil && (*s.Temperature < 0 || *s.Temperature > 2) {
		errs = append(errs, fmt.Sprintf("temperature %.2f is outside 0–2", *s.Temperature))
	}
	seen := map[string]bool{}
	for i, c := range s.Cases {
		label := fmt.Sprintf("cases[%d]", i)
		if c.Name != "" {
			label = fmt.Sprintf("cases[%d] (%s)", i, c.Name)
		}
		if strings.TrimSpace(c.Name) == "" {
			errs = append(errs, label+": name is required")
		} else if seen[c.Name] {
			// Names key results across runs; duplicates would make a diff
			// between two runs ambiguous.
			errs = append(errs, label+": duplicate case name")
		}
		seen[c.Name] = true
		if strings.TrimSpace(c.Prompt) == "" {
			errs = append(errs, label+": prompt is required")
		}
		if c.Expect.IsEmpty() {
			errs = append(errs, label+": expect must contain at least one assertion, or the case cannot fail")
		}
		for _, expr := range c.Expect.Regex {
			if _, err := regexp.Compile(expr); err != nil {
				errs = append(errs, fmt.Sprintf("%s: invalid regex %q: %v", label, expr, err))
			}
		}
		if c.Expect.MaxWords < 0 {
			errs = append(errs, label+": max_words cannot be negative")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid eval suite:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Discover finds the suite belonging to a unit directory.
//
// Resolution order is deliberately narrow — one suite, no merging — so
// that what ran is never in doubt:
//
//	evals/suite.yaml, if present
//	the only *.yaml or *.yml in evals/, if there is exactly one
//
// Anything else returns an error naming the candidates, leaving the
// choice to --suite rather than guessing.
func Discover(dir string) (string, error) {
	evalDir := filepath.Join(dir, DirName)
	entries, err := os.ReadDir(evalDir)
	if err != nil {
		return "", fmt.Errorf("no suite given and no %s/ directory in %s", DirName, dir)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(e.Name())); ext != ".yaml" && ext != ".yml" {
			continue
		}
		found = append(found, filepath.Join(evalDir, e.Name()))
	}
	sort.Strings(found)
	switch {
	case len(found) == 0:
		return "", fmt.Errorf("no suite files in %s", evalDir)
	case len(found) == 1:
		return found[0], nil
	}
	for _, f := range found {
		if filepath.Base(f) == "suite.yaml" {
			return f, nil
		}
	}
	return "", fmt.Errorf("%s holds %d suites; pick one with --suite:\n  %s",
		evalDir, len(found), strings.Join(found, "\n  "))
}
