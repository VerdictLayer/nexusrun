package bench

import (
	"strings"
	"testing"
	"time"

	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCacheRoundTrip(t *testing.T) {
	s := testStore(t)
	c, err := LoadCache(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 0 {
		t.Fatalf("a fresh store should have no entries, got %d", len(c.Entries))
	}

	c.Put(Entry{
		Unit: "code-reviewer", Version: "1.0.0", Profile: "default",
		Model: "ollama:phi3:3.8b", Backend: "llama.cpp", Device: "cpu",
		Passed: 12, Total: 13, TokPerSec: 14.0,
	})
	if err := SaveCache(s, c); err != nil {
		t.Fatal(err)
	}

	back, err := LoadCache(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Entries) != 1 {
		t.Fatalf("entries = %d", len(back.Entries))
	}
	e := back.Entries[0]
	if e.EvalScore != "12/13" {
		t.Errorf("eval_score = %q, want the rendered fraction", e.EvalScore)
	}
	if e.Timestamp.IsZero() {
		t.Error("Put should stamp the entry")
	}
	if e.Rate() < 92 || e.Rate() > 93 {
		t.Errorf("rate = %.1f", e.Rate())
	}
}

func TestPutReplacesSameKey(t *testing.T) {
	c := &Cache{}
	base := Entry{Unit: "u", Version: "1", Profile: "default", Model: "m", Passed: 5, Total: 13}
	c.Put(base)
	base.Passed = 12
	c.Put(base)
	if len(c.Entries) != 1 {
		t.Fatalf("re-measuring should replace, not append: %d entries", len(c.Entries))
	}
	if c.Entries[0].Passed != 12 {
		t.Errorf("passed = %d, want the newer measurement", c.Entries[0].Passed)
	}
}

func TestLookupHonoursTTL(t *testing.T) {
	c := &Cache{}
	k := Key{Unit: "u", Version: "1", Profile: "default", Model: "m"}
	c.Put(Entry{
		Unit: k.Unit, Version: k.Version, Profile: k.Profile, Model: k.Model,
		Passed: 12, Total: 13, Timestamp: time.Now().Add(-8 * 24 * time.Hour),
	})
	if got := c.Lookup(k, "", DefaultTTL); got != nil {
		t.Error("an 8-day-old entry should be stale under the 7-day default")
	}
	if got := c.Lookup(k, "", 30*24*time.Hour); got == nil {
		t.Error("the same entry should be fresh under a 30-day TTL")
	}
}

func TestLookupRejectsDifferentUnitDigest(t *testing.T) {
	c := &Cache{}
	k := Key{Unit: "u", Version: "1", Profile: "default", Model: "m"}
	c.Put(Entry{
		Unit: k.Unit, Version: k.Version, Profile: k.Profile, Model: k.Model,
		UnitDigest: "sha256:aaa", Passed: 12, Total: 13,
	})
	if got := c.Lookup(k, "sha256:aaa", DefaultTTL); got == nil {
		t.Error("matching digest should hit")
	}
	// A rebuilt unit with a changed prompt is a different unit; inheriting
	// its predecessor's score would report a measurement never taken.
	if got := c.Lookup(k, "sha256:bbb", DefaultTTL); got != nil {
		t.Error("a different unit digest must miss")
	}
}

func TestLookupIsKeyedOnEveryDimension(t *testing.T) {
	c := &Cache{}
	c.Put(Entry{Unit: "u", Version: "1", Profile: "default", Model: "m", Passed: 12, Total: 13})
	for name, k := range map[string]Key{
		"other unit":    {Unit: "v", Version: "1", Profile: "default", Model: "m"},
		"other version": {Unit: "u", Version: "2", Profile: "default", Model: "m"},
		"other profile": {Unit: "u", Version: "1", Profile: "fast", Model: "m"},
		"other model":   {Unit: "u", Version: "1", Profile: "default", Model: "n"},
	} {
		if got := c.Lookup(k, "", DefaultTTL); got != nil {
			t.Errorf("%s should not hit the cached entry", name)
		}
	}
}

func TestCacheFromAnotherMachineIsDiscarded(t *testing.T) {
	s := testStore(t)
	c := &Cache{
		MachineID: "sha256:some-other-machine",
		Entries: []Entry{{
			Unit: "u", Version: "1", Profile: "default", Model: "m",
			Passed: 13, Total: 13, TokPerSec: 200,
		}},
	}
	// Written verbatim, as a copied home directory would contain it:
	// SaveCache only fills in a fingerprint when there is none.
	if err := SaveCache(s, c); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCache(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Entries) != 0 {
		t.Error("results measured on another machine must not be reused here")
	}
	if back.MachineID != MachineID(nil) {
		t.Error("the reset cache should carry this machine's fingerprint")
	}
}

func TestMachineIDIsStableAndSensitive(t *testing.T) {
	hw := &hardware.Report{
		OS: "linux", Arch: "amd64", CPUModel: "Ryzen 9", CPUCores: 16,
		TotalRAMMB: 64000, CPUFeatures: []string{"avx2", "avx512"},
		Devices: []hardware.Device{{Class: "gpu", Vendor: "nvidia", Name: "RTX 4090"}},
	}
	id := MachineID(hw)
	if id != MachineID(hw) {
		t.Fatal("fingerprint is not stable")
	}
	if !strings.HasPrefix(id, "sha256:") {
		t.Errorf("id = %q", id)
	}

	// Feature order must not change the fingerprint...
	shuffled := *hw
	shuffled.CPUFeatures = []string{"avx512", "avx2"}
	if MachineID(&shuffled) != id {
		t.Error("fingerprint should not depend on CPU feature order")
	}

	// ...but swapping the accelerator must, since throughput will differ.
	noGPU := *hw
	noGPU.Devices = nil
	if MachineID(&noGPU) == id {
		t.Error("a machine without the GPU must fingerprint differently")
	}
}

func TestMachineIDToleratesRAMJitter(t *testing.T) {
	// Reported MemTotal moves slightly between boots; the cache must not
	// be invalidated by that.
	a := &hardware.Report{OS: "linux", Arch: "arm64", CPUModel: "Cortex-A72", TotalRAMMB: 3900}
	b := &hardware.Report{OS: "linux", Arch: "arm64", CPUModel: "Cortex-A72", TotalRAMMB: 3950}
	if MachineID(a) != MachineID(b) {
		t.Error("small RAM reporting differences should not change the fingerprint")
	}
}

func TestClearCache(t *testing.T) {
	s := testStore(t)
	c, _ := LoadCache(s)
	c.Put(Entry{Unit: "u", Version: "1", Profile: "default", Model: "m", Passed: 1, Total: 1})
	if err := SaveCache(s, c); err != nil {
		t.Fatal(err)
	}
	if err := ClearCache(s); err != nil {
		t.Fatal(err)
	}
	// Clearing twice must not error: it is a cleanup command.
	if err := ClearCache(s); err != nil {
		t.Fatalf("clearing an absent cache should succeed: %v", err)
	}
	back, _ := LoadCache(s)
	if len(back.Entries) != 0 {
		t.Error("cache was not cleared")
	}
}

func TestCacheStringRendersEntries(t *testing.T) {
	c := &Cache{MachineID: "sha256:abcdef0123456789"}
	c.Put(Entry{
		Unit: "code-reviewer", Version: "1.0.0", Profile: "default",
		Model: "ollama:phi3:3.8b", Backend: "llama.cpp", Device: "cpu",
		Passed: 12, Total: 13, TokPerSec: 14.0,
	})
	out := c.String()
	for _, want := range []string{"code-reviewer:1.0.0", "ollama:phi3:3.8b", "12/13", "llama.cpp/cpu"} {
		if !strings.Contains(out, want) {
			t.Errorf("table should contain %q:\n%s", want, out)
		}
	}
	if empty := (&Cache{}).String(); !strings.Contains(empty, "No benchmark results") {
		t.Errorf("empty cache message = %q", empty)
	}
}
