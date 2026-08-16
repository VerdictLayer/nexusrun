package workflow

import (
	"fmt"
	"strings"
	"text/template"
)

// Transforms reshape a payload between agents.
//
// text/template is the whole engine, with a fixed function map and nothing
// else added. That matters more than it looks: text/template has no file
// access, no process access, and no way to reach the host — the risk in a
// template engine is entirely in what you hand it, and this hands it a
// string-manipulation vocabulary and the run's own values.
//
// Note this departs from the roadmap's sketch, which wrote pipelines as
// `{{ researcher.output | summarize(100) }}`. That is not Go template
// syntax and cannot be made so without writing a second language; the same
// transform here is `{{ summarize .researcher.output 100 }}`.

// transformFuncs is the complete function vocabulary of a transform.
var transformFuncs = template.FuncMap{
	// summarize truncates to at most n characters on a word boundary,
	// marking the cut so a downstream agent is not misled into thinking it
	// received a complete document.
	"summarize": func(n int, s string) string { return summarize(s, n) },

	"trim":  strings.TrimSpace,
	"lower": strings.ToLower,
	"upper": strings.ToUpper,

	"head": func(n int, s string) string {
		lines := strings.Split(s, "\n")
		if n < len(lines) {
			lines = lines[:n]
		}
		return strings.Join(lines, "\n")
	},
	"indent": func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			if l != "" {
				lines[i] = pad + l
			}
		}
		return strings.Join(lines, "\n")
	},
	"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
	"default": func(fallback, s string) string {
		if strings.TrimSpace(s) == "" {
			return fallback
		}
		return s
	},
	"quote": func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` },
}

// CheckTransform parses a transform without executing it.
func CheckTransform(src string) error {
	_, err := compileTransform(src)
	return err
}

func compileTransform(src string) (*template.Template, error) {
	t, err := template.New("transform").Option("missingkey=error").Funcs(transformFuncs).Parse(src)
	if err != nil {
		// Template parse errors carry the internal template name, which is
		// noise to someone reading their own YAML.
		return nil, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), `template: transform:`))
	}
	return t, nil
}

// Transform renders a transform against the run environment.
func Transform(src string, env Env) (string, error) {
	t, err := compileTransform(src)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, map[string]any(env)); err != nil {
		return "", fmt.Errorf("render transform: %w", err)
	}
	// A transform is authored inside a YAML block scalar, which almost
	// always leaves a trailing newline the author did not intend as part
	// of the payload.
	return strings.TrimRight(b.String(), "\n"), nil
}

// summarize shortens text to at most n characters, cutting at the last
// word boundary so the result reads as prose rather than a severed token.
func summarize(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexAny(cut, " \n\t"); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \n\t.,;:") + "…"
}
