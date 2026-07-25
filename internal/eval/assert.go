package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Expect is what a case's output must satisfy. Every populated field is
// checked, and all of them must pass for the case to pass.
//
// The assertions are deliberately mechanical — substring, regex, shape.
// There is no model-as-judge here, and that is a real limitation rather
// than an oversight: a judge needs a second model whose own quality varies
// with the same quantization and hardware this package exists to measure,
// so it cannot be the instrument. Mechanical checks on a well-chosen
// prompt measure less, but they measure it honestly.
type Expect struct {
	// Contains requires every listed substring to appear.
	Contains []string `yaml:"contains,omitempty"`

	// NotContains requires none of the listed substrings to appear. This
	// is where refusals and hedging get caught ("I cannot", "as an AI"),
	// which is the most common way a smaller quantization fails.
	NotContains []string `yaml:"not_contains,omitempty"`

	// Regex requires every listed pattern to match somewhere in the output.
	Regex []string `yaml:"regex,omitempty"`

	// Equals requires the whole output, trimmed, to match exactly.
	Equals string `yaml:"equals,omitempty"`

	// OneOf requires at least one of the listed substrings to appear —
	// for cases with several acceptable answers.
	OneOf []string `yaml:"one_of,omitempty"`

	// JSON requires the output to parse as JSON. Fenced code blocks are
	// unwrapped first, because instruction-tuned models wrap JSON in
	// ```json fences even when told not to, and that is a formatting
	// artifact rather than the failure the case is looking for.
	JSON bool `yaml:"json,omitempty"`

	// MaxWords caps output length, for cases where brevity is the
	// requirement being tested.
	MaxWords int `yaml:"max_words,omitempty"`
}

// IsEmpty reports whether the case asserts nothing at all.
func (e Expect) IsEmpty() bool {
	return len(e.Contains) == 0 && len(e.NotContains) == 0 && len(e.Regex) == 0 &&
		e.Equals == "" && len(e.OneOf) == 0 && !e.JSON && e.MaxWords == 0
}

// Check is the outcome of one assertion.
type Check struct {
	Kind   string `json:"kind"`
	Want   string `json:"want,omitempty"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Check evaluates every assertion against the output and returns one
// result per assertion, in a stable order.
func (e Expect) Check(output string, caseSensitive bool) []Check {
	// Comparison text is folded once; the original is kept for details so
	// failure messages show what the model actually wrote.
	hay := output
	if !caseSensitive {
		hay = strings.ToLower(output)
	}
	fold := func(s string) string {
		if caseSensitive {
			return s
		}
		return strings.ToLower(s)
	}

	var checks []Check
	for _, want := range e.Contains {
		checks = append(checks, Check{
			Kind: "contains", Want: want,
			Pass: strings.Contains(hay, fold(want)),
		})
	}
	for _, want := range e.NotContains {
		checks = append(checks, Check{
			Kind: "not_contains", Want: want,
			Pass: !strings.Contains(hay, fold(want)),
		})
	}
	for _, expr := range e.Regex {
		re, err := regexp.Compile(expr)
		if err != nil {
			// Validate() compiles every pattern up front, so reaching here
			// means the suite was not validated. Fail loudly rather than
			// silently skipping the assertion.
			checks = append(checks, Check{Kind: "regex", Want: expr, Pass: false, Detail: "invalid pattern: " + err.Error()})
			continue
		}
		if !caseSensitive {
			re = regexp.MustCompile("(?i)" + expr)
		}
		checks = append(checks, Check{Kind: "regex", Want: expr, Pass: re.MatchString(output)})
	}
	if e.Equals != "" {
		got := strings.TrimSpace(output)
		want := strings.TrimSpace(e.Equals)
		checks = append(checks, Check{
			Kind: "equals", Want: want,
			Pass: fold(got) == fold(want),
		})
	}
	if len(e.OneOf) > 0 {
		matched := ""
		for _, want := range e.OneOf {
			if strings.Contains(hay, fold(want)) {
				matched = want
				break
			}
		}
		c := Check{Kind: "one_of", Want: strings.Join(e.OneOf, " | "), Pass: matched != ""}
		if matched != "" {
			c.Detail = "matched " + matched
		}
		checks = append(checks, c)
	}
	if e.JSON {
		c := Check{Kind: "json", Want: "valid JSON"}
		body := unfence(output)
		if json.Valid([]byte(body)) {
			c.Pass = true
		} else {
			c.Detail = "output is not valid JSON"
		}
		checks = append(checks, c)
	}
	if e.MaxWords > 0 {
		n := len(strings.Fields(output))
		checks = append(checks, Check{
			Kind: "max_words", Want: fmt.Sprintf("<= %d", e.MaxWords),
			Pass:   n <= e.MaxWords,
			Detail: fmt.Sprintf("got %d words", n),
		})
	}
	return checks
}

// Passed reports whether every check passed.
func Passed(checks []Check) bool {
	for _, c := range checks {
		if !c.Pass {
			return false
		}
	}
	return len(checks) > 0
}

// FailedChecks returns only the checks that failed, for reporting.
func FailedChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if !c.Pass {
			out = append(out, c)
		}
	}
	return out
}

// unfence strips a single markdown code fence wrapping the whole output,
// including the optional language tag on the opening fence.
func unfence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```")
	// Drop the language tag up to the first newline.
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	if i := strings.LastIndex(t, "```"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}
