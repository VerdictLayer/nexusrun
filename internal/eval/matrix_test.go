package eval

import (
	"strings"
	"testing"
)

func modelReport(id, model, params, quant string, sizeMB float64, override bool, passed, total int, tps float64) *Report {
	return &Report{
		ID: id, Suite: "s", Unit: "u:0.1.0",
		Model: model, ModelParams: params, ModelQuant: quant,
		ModelSize: int64(sizeMB * (1 << 20)), ModelOverride: override,
		Repeats: 1,
		Targets: []TargetResult{{
			Backend: "llama.cpp/server", Device: "cpu",
			Passed: passed, Total: total, EvalTPS: tps,
		}},
	}
}

func TestMatrixRanksByScoreThenSpeed(t *testing.T) {
	m := NewMatrix([]*Report{
		modelReport("a", "ollama:phi3", "3B", "Q4", 2200, false, 3, 4, 13.6),
		modelReport("b", "ollama:llama3.1:8b", "8B", "Q4_K_M", 4900, true, 4, 4, 8.1),
		// Same score as phi3 but faster and far smaller — the row a reader
		// is actually looking for.
		modelReport("c", "ollama:deepseek-coder", "1B", "Q4_0", 776, true, 3, 4, 38.2),
	})

	if len(m.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(m.Rows))
	}
	if m.Rows[0].Model != "ollama:llama3.1:8b" {
		t.Errorf("first row = %q, want the highest scorer", m.Rows[0].Model)
	}
	// Equal rate: the faster one ranks first, because of two models that
	// answer equally well the faster is the one to ship.
	if m.Rows[1].Model != "ollama:deepseek-coder" {
		t.Errorf("second row = %q, want the faster of the two 3/4 models", m.Rows[1].Model)
	}
	// The unit's own model is marked so a what-if is never read as the
	// unit's score.
	if !m.Rows[2].Unit || m.Rows[0].Unit {
		t.Errorf("unit-default marking wrong: %+v / %+v", m.Rows[2], m.Rows[0])
	}
	out := m.String()
	for _, want := range []string{"776 MB", "4.8 GB", "Q4_0", "8B", "100%", "75%"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered matrix omits %q:\n%s", want, out)
		}
	}
}

func TestMatrixCheapestFindsSmallestAtTopScore(t *testing.T) {
	m := NewMatrix([]*Report{
		modelReport("big", "big-model", "8B", "Q4", 4900, true, 4, 4, 8),
		modelReport("small", "small-model", "1B", "Q4", 776, true, 4, 4, 38),
		modelReport("worse", "worse-model", "1B", "Q3", 500, true, 2, 4, 40),
	})
	c := m.Cheapest()
	if c == nil || c.Model != "small-model" {
		t.Fatalf("Cheapest() = %+v, want small-model", c)
	}
	// A cheaper model that scores worse must not be recommended.
	if strings.Contains(m.String(), "worse-model matches") {
		t.Error("a lower-scoring model was presented as a match")
	}
	if !strings.Contains(m.String(), "small-model matches the best score") {
		t.Errorf("headline missing:\n%s", m.String())
	}
}

func TestMatrixKeepsUnusableModelsVisible(t *testing.T) {
	broken := modelReport("x", "broken-model", "", "", 0, true, 0, 0, 0)
	broken.Targets[0].Error = "no CUDA backend compiled in"
	m := NewMatrix([]*Report{
		modelReport("ok", "good-model", "1B", "Q4", 776, false, 4, 4, 30),
		broken,
	})
	// A model that could not run is not a zero score; it is an absence of
	// evidence, and it ranks last rather than being dropped.
	if m.Rows[1].Model != "broken-model" || m.Rows[1].Error == "" {
		t.Fatalf("unusable row = %+v, want it last with its reason", m.Rows[1])
	}
	if !strings.Contains(m.String(), "unusable: no CUDA backend compiled in") {
		t.Errorf("reason not rendered:\n%s", m.String())
	}
	if c := m.Cheapest(); c == nil || c.Model != "good-model" {
		t.Errorf("Cheapest() = %+v, want the model that actually ran", c)
	}
}
