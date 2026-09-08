package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func TestInspectReportsEverySetupConcern(t *testing.T) {
	root := t.TempDir()
	s := &store.Store{Root: root}
	for _, d := range []string{s.BlobsDir(), s.UnitsDir(), s.LogsDir(), s.EvalsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unitDir := filepath.Join(root, "unit")
	if err := os.MkdirAll(filepath.Join(unitDir, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: nexusrun.dev/v1
name: guided
version: 1.0.0
models:
  - id: main
    source: https://example.invalid/model.gguf
    size_bytes: 4294967296
    context: 2048
entrypoint:
  type: chat
hardware:
  prefer: [gpu]
mcp_servers:
  notes:
    source: file:///definitely/missing/nexusrun-test-server
secrets:
  - name: NOTES_TOKEN
    required: true
`
	suite := `apiVersion: nexusrun.dev/v1
kind: EvalSuite
name: acceptance
cases:
  - name: greets
    prompt: hello
    expect:
      contains: [hello]
`
	if err := os.WriteFile(filepath.Join(unitDir, "nexus.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "evals", "suite.yaml"), []byte(suite), 0o644); err != nil {
		t.Fatal(err)
	}
	hw := &hardware.Report{OS: "linux", Arch: "amd64", TotalRAMMB: 8192, Devices: []hardware.Device{{Class: "gpu", MemoryMB: 2048}, {Class: "cpu"}}}
	plan, err := Inspect(context.Background(), s, unitDir, Options{Hardware: hw, Capabilities: []engine.Capability{}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"model:main": ActionRequired, "runtime": ActionRequired, "hardware": Ready, "tool:notes": ActionRequired, "secret:NOTES_TOKEN": ActionRequired}
	for _, c := range plan.Checks {
		if status, ok := want[c.ID]; ok {
			if c.Status != status {
				t.Errorf("%s status=%s, want %s", c.ID, c.Status, status)
			}
			delete(want, c.ID)
			if c.ID == "model:main" && c.SizeBytes != 4294967296 {
				t.Errorf("model size=%d", c.SizeBytes)
			}
		}
	}
	for id := range want {
		t.Errorf("missing check %s", id)
	}
	if plan.Configuration == nil || plan.Configuration.Device != "cpu" {
		t.Fatalf("configuration=%+v, want CPU fallback", plan.Configuration)
	}
	if plan.Tests == nil || plan.Tests.Suite != "acceptance" || plan.Tests.Status != Pending {
		t.Fatalf("tests=%+v", plan.Tests)
	}
}

func TestInspectMarksCachedLocalModelReady(t *testing.T) {
	root := t.TempDir()
	s := &store.Store{Root: root}
	for _, d := range []string{s.BlobsDir(), s.UnitsDir(), s.LogsDir(), s.EvalsDir()} {
		os.MkdirAll(d, 0o755)
	}
	model := filepath.Join(root, "tiny.gguf")
	os.WriteFile(model, []byte("weights"), 0o644)
	dir := filepath.Join(root, "unit")
	os.MkdirAll(dir, 0o755)
	body := "apiVersion: nexusrun.dev/v1\nname: local\nversion: 1\nmodels:\n  - source: " + model + "\nentrypoint:\n  type: chat\n"
	os.WriteFile(filepath.Join(dir, "nexus.yaml"), []byte(body), 0o644)
	hw := &hardware.Report{OS: "linux", Arch: "amd64", TotalRAMMB: 1024, Devices: []hardware.Device{{Class: "cpu"}}}
	caps := []engine.Capability{{Backend: "llama.cpp", Available: true, AcceptsModelPath: true, Devices: []string{"cpu"}}}
	p, err := Inspect(context.Background(), s, dir, Options{Hardware: hw, Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	if p.Checks[0].Status != Ready || p.Checks[0].SizeBytes != 7 {
		t.Fatalf("model check=%+v", p.Checks[0])
	}
}
