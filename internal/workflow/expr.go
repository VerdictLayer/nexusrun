package workflow

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file is a small expression evaluator for routing conditions.
//
// The obvious move is to import a general expression library. The reason
// not to is the same reason NexusRun ships one static binary: a routing
// condition needs comparisons, a handful of string predicates, and boolean
// combination — perhaps two hundred lines — and a general evaluator brings
// a language, including the parts (indexing, method calls, arbitrary
// arithmetic on values pulled from a model's output) that turn a condition
// into an execution vector. What is not implemented cannot be exploited.
//
// The grammar, loosest to tightest:
//
//	expr    := or
//	or      := and (("or" | "||") and)*
//	and     := cmp (("and" | "&&") cmp)*
//	cmp     := unary (("==" | "!=" | "<" | "<=" | ">" | ">=") unary)?
//	unary   := ("not" | "!") unary | primary
//	primary := number | string | ident ("." ident)* | call | "(" expr ")"
//	call    := ident "(" [expr ("," expr)*] ")"

// Env is the value namespace a condition sees: agent name → fields, plus
// the workflow's own top-level values.
type Env map[string]any

// Fields an agent exposes to conditions and transforms.
const (
	FieldOutput    = "output"
	FieldTokens    = "tokens"
	FieldTokPerSec = "tok_per_sec"
	FieldTookMS    = "took_ms"
	FieldDevice    = "device"
	FieldBackend   = "backend"
	FieldOK        = "ok"
)

// agentFields is the field set every agent value carries. Conditions are
// validated against it, so `researcher.putput` is caught by
// `nexus compose validate` rather than silently evaluating to nothing.
var agentFields = map[string]bool{
	FieldOutput: true, FieldTokens: true, FieldTokPerSec: true,
	FieldTookMS: true, FieldDevice: true, FieldBackend: true, FieldOK: true,
}

// Eval evaluates a condition to a boolean.
func Eval(expr string, env Env) (bool, error) {
	p := &parser{src: expr}
	v, err := p.parse()
	if err != nil {
		return false, err
	}
	return truthy(v, env)
}

// CheckCondition parses a condition and checks every name it mentions
// against the workflow's agents, without evaluating anything.
func CheckCondition(expr string, agents []string) error {
	p := &parser{src: expr}
	node, err := p.parse()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, a := range agents {
		known[a] = true
	}
	return node.check(known)
}

// --- values ---------------------------------------------------------------

func truthy(n node, env Env) (bool, error) {
	v, err := n.eval(env)
	if err != nil {
		return false, err
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		return t != "", nil
	case float64:
		return t != 0, nil
	case nil:
		return false, nil
	default:
		return false, fmt.Errorf("condition produced %T, which is not a truth value", v)
	}
}

// --- AST ------------------------------------------------------------------

type node interface {
	eval(Env) (any, error)
	check(known map[string]bool) error
}

type literal struct{ v any }

func (l literal) eval(Env) (any, error)       { return l.v, nil }
func (l literal) check(map[string]bool) error { return nil }

// ref is a dotted name: either a bare identifier or agent.field.
type ref struct {
	parts []string
}

func (r ref) eval(env Env) (any, error) {
	// A bare name resolves against the top-level environment. It is how
	// `always` and `input` work without being keywords.
	if len(r.parts) == 1 {
		v, ok := env[r.parts[0]]
		if !ok {
			return nil, fmt.Errorf("unknown name %q", r.parts[0])
		}
		return v, nil
	}
	head, ok := env[r.parts[0]]
	if !ok {
		// An agent that has not run yet has no value. This is not an error:
		// a route whose condition mentions a stage that was skipped should
		// simply not fire.
		return nil, nil
	}
	m, ok := head.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q has no fields", r.parts[0])
	}
	cur := m
	for i, p := range r.parts[1:] {
		v, ok := cur[p]
		if !ok {
			return nil, fmt.Errorf("%s has no field %q", strings.Join(r.parts[:i+1], "."), p)
		}
		if i == len(r.parts)-2 {
			return v, nil
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is not a record", strings.Join(r.parts[:i+2], "."))
		}
		cur = next
	}
	return nil, nil
}

func (r ref) check(known map[string]bool) error {
	switch len(r.parts) {
	case 1:
		switch r.parts[0] {
		case CondAlways, "never", "input", "true", "false":
			return nil
		}
		if known[r.parts[0]] {
			return fmt.Errorf("%q is an agent; use %s.%s to read what it produced", r.parts[0], r.parts[0], FieldOutput)
		}
		return fmt.Errorf("unknown name %q", r.parts[0])
	case 2:
		if !known[r.parts[0]] {
			return fmt.Errorf("%q is not an agent in this workflow", r.parts[0])
		}
		if !agentFields[r.parts[1]] {
			return fmt.Errorf("agents have no field %q (available: %s)", r.parts[1], strings.Join(sortedFields(), ", "))
		}
		return nil
	default:
		return fmt.Errorf("%q is too deeply nested; conditions read agent.field", strings.Join(r.parts, "."))
	}
}

func sortedFields() []string {
	return []string{FieldOutput, FieldTokens, FieldTokPerSec, FieldTookMS, FieldDevice, FieldBackend, FieldOK}
}

type binary struct {
	op   string
	l, r node
}

func (b binary) check(known map[string]bool) error {
	if err := b.l.check(known); err != nil {
		return err
	}
	return b.r.check(known)
}

func (b binary) eval(env Env) (any, error) {
	// Short-circuit, so `x.ok and x.output == "y"` is safe when x never ran.
	switch b.op {
	case "and":
		lt, err := truthy(b.l, env)
		if err != nil || !lt {
			return false, err
		}
		return truthy(b.r, env)
	case "or":
		lt, err := truthy(b.l, env)
		if err != nil {
			return false, err
		}
		if lt {
			return true, nil
		}
		return truthy(b.r, env)
	}

	lv, err := b.l.eval(env)
	if err != nil {
		return nil, err
	}
	rv, err := b.r.eval(env)
	if err != nil {
		return nil, err
	}
	switch b.op {
	case "==":
		return equal(lv, rv), nil
	case "!=":
		return !equal(lv, rv), nil
	}

	ln, lok := asNumber(lv)
	rn, rok := asNumber(rv)
	if !lok || !rok {
		return nil, fmt.Errorf("cannot compare %s with %s using %s", typeName(lv), typeName(rv), b.op)
	}
	switch b.op {
	case "<":
		return ln < rn, nil
	case "<=":
		return ln <= rn, nil
	case ">":
		return ln > rn, nil
	case ">=":
		return ln >= rn, nil
	}
	return nil, fmt.Errorf("unknown operator %q", b.op)
}

type unary struct {
	op string
	x  node
}

func (u unary) check(known map[string]bool) error { return u.x.check(known) }

func (u unary) eval(env Env) (any, error) {
	t, err := truthy(u.x, env)
	if err != nil {
		return nil, err
	}
	return !t, nil
}

type call struct {
	name string
	args []node
}

func (c call) check(known map[string]bool) error {
	fn, ok := functions[c.name]
	if !ok {
		return fmt.Errorf("unknown function %q (available: %s)", c.name, strings.Join(functionNames(), ", "))
	}
	if len(c.args) != fn.arity {
		return fmt.Errorf("%s() takes %d argument(s), got %d", c.name, fn.arity, len(c.args))
	}
	for _, a := range c.args {
		if err := a.check(known); err != nil {
			return err
		}
	}
	return nil
}

func (c call) eval(env Env) (any, error) {
	fn, ok := functions[c.name]
	if !ok {
		return nil, fmt.Errorf("unknown function %q", c.name)
	}
	if len(c.args) != fn.arity {
		return nil, fmt.Errorf("%s() takes %d argument(s), got %d", c.name, fn.arity, len(c.args))
	}
	vals := make([]any, len(c.args))
	for i, a := range c.args {
		v, err := a.eval(env)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return fn.fn(vals)
}

// --- functions ------------------------------------------------------------

type builtin struct {
	arity int
	fn    func([]any) (any, error)
}

// functions is the complete vocabulary available to a condition. Adding to
// it is a deliberate act; there is no reflection-based escape hatch.
var functions = map[string]builtin{
	"len": {1, func(a []any) (any, error) {
		return float64(len(asString(a[0]))), nil
	}},
	"contains": {2, func(a []any) (any, error) {
		return strings.Contains(asString(a[0]), asString(a[1])), nil
	}},
	"matches": {2, func(a []any) (any, error) {
		re, err := regexp.Compile(asString(a[1]))
		if err != nil {
			return nil, fmt.Errorf("matches(): %w", err)
		}
		return re.MatchString(asString(a[0])), nil
	}},
	"lower": {1, func(a []any) (any, error) { return strings.ToLower(asString(a[0])), nil }},
	"upper": {1, func(a []any) (any, error) { return strings.ToUpper(asString(a[0])), nil }},
	"trim":  {1, func(a []any) (any, error) { return strings.TrimSpace(asString(a[0])), nil }},
	"words": {1, func(a []any) (any, error) {
		return float64(len(strings.Fields(asString(a[0])))), nil
	}},
	"lines": {1, func(a []any) (any, error) {
		s := strings.TrimRight(asString(a[0]), "\n")
		if s == "" {
			return float64(0), nil
		}
		return float64(strings.Count(s, "\n") + 1), nil
	}},
}

func functionNames() []string {
	names := make([]string, 0, len(functions))
	for n := range functions {
		names = append(names, n)
	}
	// Fixed order: this string ends up in error messages that are diffed.
	sortStrings(names)
	return names
}

// --- coercion -------------------------------------------------------------

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func asNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func equal(a, b any) bool {
	if an, aok := asNumber(a); aok {
		if bn, bok := asNumber(b); bok {
			return an == bn
		}
	}
	// Anything else compares as text, which is what a condition against a
	// model's output almost always means.
	return asString(a) == asString(b)
}

func typeName(v any) string {
	switch v.(type) {
	case string:
		return "text"
	case float64, int:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "nothing"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// --- parser ---------------------------------------------------------------

type parser struct {
	src string
	pos int
}

func (p *parser) parse() (node, error) {
	if strings.TrimSpace(p.src) == "" {
		return nil, fmt.Errorf("empty condition")
	}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos < len(p.src) {
		return nil, fmt.Errorf("unexpected %q at position %d", p.src[p.pos:], p.pos)
	}
	return n, nil
}

func (p *parser) parseOr() (node, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		if !p.acceptOp("or", "||") {
			return l, nil
		}
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = binary{op: "or", l: l, r: r}
	}
}

func (p *parser) parseAnd() (node, error) {
	l, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for {
		if !p.acceptOp("and", "&&") {
			return l, nil
		}
		r, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		l = binary{op: "and", l: l, r: r}
	}
}

// cmpOps is ordered longest-first so "<=" is not read as "<" then "=".
var cmpOps = []string{"==", "!=", "<=", ">=", "<", ">"}

func (p *parser) parseCmp() (node, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	for _, op := range cmpOps {
		if strings.HasPrefix(p.src[p.pos:], op) {
			p.pos += len(op)
			r, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			return binary{op: op, l: l, r: r}, nil
		}
	}
	return l, nil
}

func (p *parser) parseUnary() (node, error) {
	p.skipSpace()
	if p.acceptOp("not", "!") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return unary{op: "not", x: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("unexpected end of condition")
	}
	c := p.src[p.pos]

	if c == '(' {
		p.pos++
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ')' {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.pos++
		return n, nil
	}
	if c == '"' || c == '\'' {
		return p.parseString(c)
	}
	if c >= '0' && c <= '9' {
		return p.parseNumber()
	}
	if isIdentStart(c) {
		return p.parseIdent()
	}
	return nil, fmt.Errorf("unexpected character %q at position %d", string(c), p.pos)
}

func (p *parser) parseString(quote byte) (node, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\\' && p.pos+1 < len(p.src) {
			p.pos++
			switch p.src[p.pos] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(p.src[p.pos])
			}
			p.pos++
			continue
		}
		if c == quote {
			p.pos++
			return literal{v: b.String()}, nil
		}
		b.WriteByte(c)
		p.pos++
	}
	return nil, fmt.Errorf("unterminated string")
}

func (p *parser) parseNumber() (node, error) {
	start := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] >= '0' && p.src[p.pos] <= '9' || p.src[p.pos] == '.' || p.src[p.pos] == '_') {
		p.pos++
	}
	// Underscores are allowed as digit separators (100_000) and stripped.
	text := strings.ReplaceAll(p.src[start:p.pos], "_", "")
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, fmt.Errorf("bad number %q", p.src[start:p.pos])
	}
	return literal{v: f}, nil
}

func (p *parser) parseIdent() (node, error) {
	var parts []string
	for {
		start := p.pos
		for p.pos < len(p.src) && isIdentPart(p.src[p.pos]) {
			p.pos++
		}
		if start == p.pos {
			return nil, fmt.Errorf("expected a name at position %d", p.pos)
		}
		parts = append(parts, p.src[start:p.pos])

		// A '(' directly after a single name makes it a call.
		if len(parts) == 1 && p.pos < len(p.src) && p.src[p.pos] == '(' {
			return p.parseCall(parts[0])
		}
		if p.pos < len(p.src) && p.src[p.pos] == '.' {
			p.pos++
			continue
		}
		break
	}

	switch strings.Join(parts, ".") {
	case "true":
		return literal{v: true}, nil
	case "false":
		return literal{v: false}, nil
	}
	return ref{parts: parts}, nil
}

func (p *parser) parseCall(name string) (node, error) {
	p.pos++ // '('
	c := call{name: name}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ')' {
		p.pos++
		return c, nil
	}
	for {
		arg, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		c.args = append(c.args, arg)
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("unterminated call to %s()", name)
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ')':
			p.pos++
			return c, nil
		default:
			return nil, fmt.Errorf("expected ',' or ')' in %s() at position %d", name, p.pos)
		}
	}
}

// acceptOp consumes the first matching operator spelling. Word operators
// must be followed by a non-identifier character, so an agent named
// "android" is not read as "and" plus "roid".
func (p *parser) acceptOp(word, symbol string) bool {
	p.skipSpace()
	rest := p.src[p.pos:]
	if strings.HasPrefix(rest, symbol) {
		p.pos += len(symbol)
		return true
	}
	if strings.HasPrefix(rest, word) {
		after := p.pos + len(word)
		if after >= len(p.src) || !isIdentPart(p.src[after]) {
			p.pos = after
			return true
		}
	}
	return false
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
