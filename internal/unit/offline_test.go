package unit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/store"
)

func TestValidateOfflineRequiresSealedWeightsAndBundledRuntime(t *testing.T) {
	root := t.TempDir()
	s := &store.Store{Root: filepath.Join(root, "store")}
	for _, d := range []string{s.BlobsDir(), s.UnitsDir(), s.LogsDir(), s.EvalsDir()} {
		os.MkdirAll(d, 0o755)
	}
	dir := filepath.Join(root, "unit")
	os.MkdirAll(filepath.Join(dir, "runtime"), 0o755)
	model := filepath.Join(dir, "model.gguf")
	os.WriteFile(model, []byte("weights"), 0o644)
	os.WriteFile(filepath.Join(dir, "runtime", "llama-cli"), []byte("binary"), 0o755)
	body := `apiVersion: nexusrun.dev/v1
name: offline
version: 1.0.0
models:
  - source: ./model.gguf
entrypoint:
  type: chat
installation:
  targets:
    - os: linux
      arch: amd64
      device: cpu
  runtime:
    name: llama.cpp
    version: b1
    path: runtime
`
	os.WriteFile(filepath.Join(dir, "nexus.yaml"), []byte(body), 0o644)
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old) // local model sources are relative to the invoking directory
	if _, err := Build(context.Background(), s, dir, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	err := ValidateOffline(context.Background(), s, "offline:1.0.0", "linux/amd64/cpu")
	if err == nil || !strings.Contains(err.Error(), "not sealed") {
		t.Fatalf("linked validation error=%v", err)
	}
	if _, err := Build(context.Background(), s, dir, BuildOptions{Seal: true}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOffline(context.Background(), s, "offline:1.0.0", "linux/amd64/cpu"); err != nil {
		t.Fatalf("sealed package rejected: %v", err)
	}
}

// A fleet host tests a release before activating it, so a tool may run under
// a host interpreter; a bundle has no such gate and must carry everything. The
// unit's own script must still be present and inside the unit either way.
func TestFleetValidationAllowsHostInterpretersButNotMissingOrEscapingScripts(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		fleetOK       bool
	}{
		{"interpreter-and-script", `["/bin/sh", "tools/lookup.sh"]`, true},
		{"bare-interpreter", `["python3", "tools/lookup.sh"]`, true},
		{"unit-relative", `["tools/lookup.sh"]`, true},
		{"missing-script", `["/bin/sh", "tools/missing.sh"]`, false},
		{"escaping-script", `["/bin/sh", "../outside.sh"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s := &store.Store{Root: filepath.Join(root, "store")}
			for _, d := range []string{s.BlobsDir(), s.UnitsDir(), s.LogsDir(), s.EvalsDir()} {
				os.MkdirAll(d, 0o755)
			}
			dir := filepath.Join(root, "unit")
			os.MkdirAll(filepath.Join(dir, "tools"), 0o755)
			os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("weights"), 0o644)
			os.WriteFile(filepath.Join(dir, "tools", "lookup.sh"), []byte("echo ok\n"), 0o755)
			body := `apiVersion: nexusrun.dev/v1
name: tooled
version: 1.0.0
models:
  - source: ./model.gguf
entrypoint:
  type: chat
tools:
  - name: lookup
    description: Look something up
    exec:
      type: script
      command: ` + tc.command + `
installation:
  targets:
    - os: linux
      arch: amd64
      device: cpu
`
			os.WriteFile(filepath.Join(dir, "nexus.yaml"), []byte(body), 0o644)
			old, _ := os.Getwd()
			os.Chdir(dir)
			defer os.Chdir(old)
			if _, err := Build(context.Background(), s, dir, BuildOptions{Seal: true}); err != nil {
				t.Fatal(err)
			}
			if err := ValidateOfflineAgent(context.Background(), s, "tooled:1.0.0", "linux/amd64/cpu"); err == nil {
				t.Fatal("a bundle accepted packaged tools")
			}
			err := ValidateFleetAgent(context.Background(), s, "tooled:1.0.0", "linux/amd64/cpu")
			if (err == nil) != tc.fleetOK {
				t.Fatalf("fleet validation: want ok=%v, got %v", tc.fleetOK, err)
			}
		})
	}
}
