package workflow

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	yamlLine    = regexp.MustCompile(`line (\d+):? ?`)
	unknownProp = regexp.MustCompile(`field (\S+) not found in type workflow\.\w+`)
)

// Diagnose parses workflow bytes and reports every problem it can find, YAML
// syntax and unknown fields included, each with a location. It is what an editor
// wants: a spec to draw even when the file is not yet valid, and a list of
// problems to show next to it. The spec is nil only when the YAML itself could
// not be read.
func Diagnose(data []byte) (*Spec, []Diagnostic) {
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		var ds []Diagnostic
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, m := range te.Errors {
				ds = append(ds, yamlDiagnostic(m))
			}
			// A type error still leaves a usable spec for everything that did decode.
			return &s, append(ds, s.Diagnostics()...)
		}
		return nil, []Diagnostic{yamlDiagnostic(strings.TrimPrefix(err.Error(), "yaml: "))}
	}
	ds := s.Diagnostics()
	if len(ds) == 0 {
		s.applyDefaults()
	}
	return &s, ds
}

func yamlDiagnostic(msg string) Diagnostic {
	d := Diagnostic{}
	if m := yamlLine.FindStringSubmatch(msg); m != nil {
		d.Line, _ = strconv.Atoi(m[1])
		msg = strings.TrimSpace(yamlLine.ReplaceAllString(msg, ""))
	}
	d.Message = unknownProp.ReplaceAllString(msg, `unknown field "$1"`)
	return d
}
