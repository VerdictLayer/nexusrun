package eval

import "testing"

func TestExpectAssertions(t *testing.T) {
	tests := []struct {
		name          string
		expect        Expect
		output        string
		caseSensitive bool
		want          bool
	}{
		{
			name:   "contains all",
			expect: Expect{Contains: []string{"E_TIMEOUT", "retry"}},
			output: "The log shows E_TIMEOUT; the client should retry.",
			want:   true,
		},
		{
			name:   "contains missing one",
			expect: Expect{Contains: []string{"E_TIMEOUT", "retry"}},
			output: "The log shows E_TIMEOUT.",
			want:   false,
		},
		// Models vary casing freely, so folding is the default. A suite
		// that fails because the model wrote "Timeout" is measuring
		// nothing about the unit.
		{
			name:   "contains folds case by default",
			expect: Expect{Contains: []string{"E_TIMEOUT"}},
			output: "the log shows e_timeout",
			want:   true,
		},
		{
			name:          "contains respects case when asked",
			expect:        Expect{Contains: []string{"E_TIMEOUT"}},
			output:        "the log shows e_timeout",
			caseSensitive: true,
			want:          false,
		},
		// Refusals and hedging are the most common failure mode of a
		// smaller quantization, and this is how they get caught.
		{
			name:   "not_contains catches a refusal",
			expect: Expect{NotContains: []string{"I cannot", "as an AI"}},
			output: "I cannot help with that request.",
			want:   false,
		},
		{
			name:   "not_contains passes when absent",
			expect: Expect{NotContains: []string{"I cannot"}},
			output: "The error code is E_TIMEOUT.",
			want:   true,
		},
		{
			name:   "regex matches",
			expect: Expect{Regex: []string{`E_[A-Z]+`}},
			output: "found E_TIMEOUT in the log",
			want:   true,
		},
		{
			name:   "regex folds case by default",
			expect: Expect{Regex: []string{`E_[A-Z]+`}},
			output: "found e_timeout in the log",
			want:   true,
		},
		{
			name:          "regex respects case when asked",
			expect:        Expect{Regex: []string{`E_[A-Z]+`}},
			output:        "found e_timeout in the log",
			caseSensitive: true,
			want:          false,
		},
		{
			name:   "equals ignores surrounding whitespace",
			expect: Expect{Equals: "yes"},
			output: "  yes\n",
			want:   true,
		},
		{
			name:   "equals rejects extra prose",
			expect: Expect{Equals: "yes"},
			output: "yes, definitely",
			want:   false,
		},
		{
			name:   "one_of accepts any alternative",
			expect: Expect{OneOf: []string{"critical", "fatal", "severe"}},
			output: "This looks fatal.",
			want:   true,
		},
		{
			name:   "one_of fails when none match",
			expect: Expect{OneOf: []string{"critical", "fatal"}},
			output: "This looks fine.",
			want:   false,
		},
		{
			name:   "json accepts plain object",
			expect: Expect{JSON: true},
			output: `{"code":"E_TIMEOUT","retry":true}`,
			want:   true,
		},
		// Instruction-tuned models fence JSON even when told not to. That
		// is a formatting artifact, not the failure the case is testing.
		{
			name:   "json accepts fenced object",
			expect: Expect{JSON: true},
			output: "```json\n{\"code\":\"E_TIMEOUT\"}\n```",
			want:   true,
		},
		{
			name:   "json rejects prose",
			expect: Expect{JSON: true},
			output: "Sure! Here is the JSON you asked for.",
			want:   false,
		},
		{
			name:   "max_words enforces brevity",
			expect: Expect{MaxWords: 5},
			output: "one two three four five six",
			want:   false,
		},
		{
			name:   "max_words passes under the cap",
			expect: Expect{MaxWords: 5},
			output: "one two three",
			want:   true,
		},
		// Every populated assertion must hold, not just one.
		{
			name:   "assertions are conjunctive",
			expect: Expect{Contains: []string{"E_TIMEOUT"}, NotContains: []string{"I cannot"}},
			output: "I cannot tell you, but it is E_TIMEOUT.",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := tt.expect.Check(tt.output, tt.caseSensitive)
			if got := Passed(checks); got != tt.want {
				t.Errorf("Passed() = %v, want %v (checks: %+v)", got, tt.want, checks)
			}
		})
	}
}

// An empty Expect must never report a pass: a case that asserts nothing
// would otherwise inflate every score it appears in.
func TestEmptyExpectNeverPasses(t *testing.T) {
	var e Expect
	if !e.IsEmpty() {
		t.Fatal("IsEmpty() = false for a zero Expect")
	}
	if Passed(e.Check("anything at all", false)) {
		t.Error("an empty Expect reported a pass")
	}
}

func TestFailedChecksReportsOnlyFailures(t *testing.T) {
	e := Expect{Contains: []string{"present", "absent"}}
	failed := FailedChecks(e.Check("this one is present", false))
	if len(failed) != 1 {
		t.Fatalf("got %d failures, want 1: %+v", len(failed), failed)
	}
	if failed[0].Want != "absent" {
		t.Errorf("failure names %q, want %q", failed[0].Want, "absent")
	}
}

func TestUnfence(t *testing.T) {
	tests := []struct{ in, want string }{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\nplain\n```", "plain"},
		{"no fence", "no fence"},
		{"  {\"a\":1}  ", `{"a":1}`},
	}
	for _, tt := range tests {
		if got := unfence(tt.in); got != tt.want {
			t.Errorf("unfence(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
