package eval

import (
	"fmt"
	"sort"
	"strings"
)

// A Matrix is one suite scored across several models.
//
// This is the question people actually have about a local agent and cannot
// currently answer: not "is this model good" in the abstract, but "is the
// cheap one good enough for *this* job". A 700 MB model that passes the same
// suite as an 8 GB one is worth knowing about, and so is the opposite —
// either way it is a fact about a specific agent on specific hardware, which
// is the only form the answer can honestly take.
//
// It is deliberately a rendering over ordinary reports rather than a new kind
// of run: every row is a full, saved, separately citable evaluation.
type Matrix struct {
	Suite   string  `json:"suite"`
	Unit    string  `json:"unit,omitempty"`
	Rows    []Row   `json:"rows"`
	Host    string  `json:"host,omitempty"`
	Repeats int     `json:"repeats"`
	Temp    float64 `json:"temperature"`
}

// Row is one model's result, flattened from its report.
type Row struct {
	ReportID string  `json:"report_id"`
	Model    string  `json:"model"`
	Params   string  `json:"params,omitempty"`
	Quant    string  `json:"quant,omitempty"`
	SizeMB   float64 `json:"size_mb,omitempty"`
	Target   string  `json:"target"`
	Passed   int     `json:"passed"`
	Total    int     `json:"total"`
	Rate     float64 `json:"rate"`
	Flaky    int     `json:"flaky"`
	EvalTPS  float64 `json:"eval_tokens_per_sec"`
	Unit     bool    `json:"unit_default,omitempty"` // the unit's own model
	Error    string  `json:"error,omitempty"`
}

// NewMatrix flattens reports into a comparison, best score first.
func NewMatrix(reports []*Report) *Matrix {
	m := &Matrix{}
	for _, r := range reports {
		if r == nil {
			continue
		}
		if m.Suite == "" {
			m.Suite, m.Unit, m.Repeats, m.Temp = r.Suite, r.Unit, r.Repeats, r.Temperature
			if r.Host != nil {
				m.Host = fmt.Sprintf("%s/%s · %s", r.Host.OS, r.Host.Arch, r.Host.CPUModel)
			}
		}
		row := Row{
			ReportID: r.ID,
			Model:    r.Model,
			Params:   r.ModelParams,
			Quant:    r.ModelQuant,
			Target:   "—",
			Unit:     !r.ModelOverride,
		}
		if r.ModelSize > 0 {
			row.SizeMB = float64(r.ModelSize) / (1 << 20)
		}
		best := r.Best()
		if best == nil {
			row.Error = "no target could execute the suite"
			if len(r.Targets) > 0 && r.Targets[0].Error != "" {
				row.Error = firstLine(r.Targets[0].Error)
			}
			m.Rows = append(m.Rows, row)
			continue
		}
		row.Target = best.Label()
		row.Passed, row.Total, row.Flaky = best.Passed, best.Total, best.Flaky
		row.Rate, row.EvalTPS = best.Rate(), best.EvalTPS
		m.Rows = append(m.Rows, row)
	}

	// Rank by score, then by speed: of two models that answer equally well,
	// the faster one is the one to ship.
	sort.SliceStable(m.Rows, func(i, j int) bool {
		a, b := m.Rows[i], m.Rows[j]
		if (a.Error == "") != (b.Error == "") {
			return a.Error == ""
		}
		if a.Rate != b.Rate {
			return a.Rate > b.Rate
		}
		return a.EvalTPS > b.EvalTPS
	})
	return m
}

// Cheapest returns the smallest model that scored as well as the best one,
// or nil when sizes are unknown. It is the row the reader is looking for:
// the least model that still does the job.
func (m *Matrix) Cheapest() *Row {
	var top *Row
	for i := range m.Rows {
		if m.Rows[i].Error == "" {
			top = &m.Rows[i]
			break
		}
	}
	if top == nil {
		return nil
	}
	best := top
	for i := range m.Rows {
		r := &m.Rows[i]
		if r.Error != "" || r.Rate != top.Rate || r.SizeMB == 0 {
			continue
		}
		if best.SizeMB == 0 || r.SizeMB < best.SizeMB {
			best = r
		}
	}
	return best
}

// String renders the comparison.
func (m *Matrix) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Suite:    %s", m.Suite)
	if m.Repeats > 1 {
		fmt.Fprintf(&b, " (× %d repeats)", m.Repeats)
	}
	b.WriteString("\n")
	if m.Unit != "" {
		fmt.Fprintf(&b, "Unit:     %s\n", m.Unit)
	}
	if m.Host != "" {
		fmt.Fprintf(&b, "Host:     %s\n", m.Host)
	}
	fmt.Fprintf(&b, "Sampling: temperature %.2f\n\n", m.Temp)

	nameW := len("MODEL")
	for _, r := range m.Rows {
		nameW = max(nameW, len(r.Model)+2) // room for the unit marker
	}
	head := fmt.Sprintf("  %%-%ds %%-7s %%-8s %%9s %%8s %%7s %%9s\n", nameW)
	fmt.Fprintf(&b, head, "MODEL", "PARAMS", "QUANT", "SIZE", "PASS", "RATE", "tok/s")
	fmt.Fprintf(&b, "  %s\n", strings.Repeat("─", nameW+54))

	for _, r := range m.Rows {
		name := r.Model
		if r.Unit {
			// The unit's own model, as opposed to one passed on the command
			// line — the difference between the unit's score and a what-if.
			name += " *"
		}
		if r.Error != "" {
			fmt.Fprintf(&b, "  %-*s %s\n", nameW, name, "unusable: "+r.Error)
			continue
		}
		size := "—"
		if r.SizeMB > 0 {
			size = humanMB(r.SizeMB)
		}
		fmt.Fprintf(&b, head, name, dash(r.Params), dash(r.Quant), size,
			fmt.Sprintf("%d/%d", r.Passed, r.Total), fmt.Sprintf("%.0f%%", r.Rate),
			fmt.Sprintf("%.1f", r.EvalTPS))
	}

	if m.Unit != "" {
		b.WriteString("\n  * the unit's own model; the rest were supplied with --model\n")
	}

	// The headline, when there is one: the smallest and largest models that
	// both reached the top score. That gap is the finding — how much model
	// this particular job actually needs. Comparing against the first row
	// instead would almost never say anything, because at equal scores the
	// smaller model is usually the faster one and already sorts first.
	if small, large := m.extremesAtTopRate(); small != nil && large != nil && small.Model != large.Model {
		fmt.Fprintf(&b, "\n%s matches the best score (%.0f%%) at %s — %.1f× smaller than %s, and %.1f× faster.\n",
			small.Model, small.Rate, humanMB(small.SizeMB), large.SizeMB/small.SizeMB, large.Model,
			small.EvalTPS/max(large.EvalTPS, 0.0001))
	}
	return b.String()
}

// extremesAtTopRate returns the smallest and largest models that achieved
// the best score, when sizes are known for both.
func (m *Matrix) extremesAtTopRate() (small, large *Row) {
	top := m.Cheapest()
	if top == nil {
		return nil, nil
	}
	for i := range m.Rows {
		r := &m.Rows[i]
		if r.Error != "" || r.Rate != top.Rate || r.SizeMB == 0 {
			continue
		}
		if small == nil || r.SizeMB < small.SizeMB {
			small = r
		}
		if large == nil || r.SizeMB > large.SizeMB {
			large = r
		}
	}
	return small, large
}

func humanMB(mb float64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024)
	}
	return fmt.Sprintf("%.0f MB", mb)
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
