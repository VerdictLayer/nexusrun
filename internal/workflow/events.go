package workflow

// Event is one step of a workflow run, reported as it happens. A tool that
// draws a run (the Studio canvas) reads these instead of parsing log lines.
type Event struct {
	// Kind is one of: start, route, agent_start, agent_done, agent_skipped,
	// agent_failed, done.
	Kind string `json:"event"`

	Agent string `json:"agent,omitempty"`
	Unit  string `json:"unit,omitempty"`

	// start: the order agents will run in.
	Order []string `json:"order,omitempty"`

	// route: whether the link delivered anything to its target.
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Fired *bool  `json:"fired,omitempty"`

	// agent_start and agent_done.
	Prompt    string  `json:"prompt,omitempty"`
	Output    string  `json:"output,omitempty"`
	Tokens    int     `json:"tokens,omitempty"`
	TokPerSec float64 `json:"tokens_per_sec,omitempty"`
	Backend   string  `json:"backend,omitempty"`
	Device    string  `json:"device,omitempty"`
	Attempts  int     `json:"attempts,omitempty"`
	TookMS    int64   `json:"took_ms,omitempty"`

	// agent_skipped, agent_failed and done.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// OK is set on done.
	OK *bool `json:"ok,omitempty"`
}

func flag(b bool) *bool { return &b }
