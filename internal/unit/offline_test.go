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
