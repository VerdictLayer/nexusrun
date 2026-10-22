package unit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/store"
)

func TestFleetVerifyDetectsLocalLayerCorruption(t *testing.T) {
	s, err := store.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "nexus.yaml"), `apiVersion: nexusrun.dev/v1
name: verification-test
version: 0.1.0
models:
  - id: main
    source: ./model.gguf
entrypoint:
  type: chat
`)
	write(t, filepath.Join(src, "model.gguf"), "dummy weights for a byte-integrity test")
	t.Chdir(src)
	built, err := Build(context.Background(), s, src, BuildOptions{Seal: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = Verify(context.Background(), s, built.Ref); err != nil {
		t.Fatal(err)
	}
	_, manifest, err := Resolve(context.Background(), s, built.Ref)
	if err != nil {
		t.Fatal(err)
	}
	layer := manifest.Layers[len(manifest.Layers)-1]
	path := filepath.Join(s.UnitsDir(), "blobs", "sha256", strings.TrimPrefix(layer.Digest.String(), "sha256:"))
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = Verify(context.Background(), s, built.Ref); err == nil {
		t.Fatal("accepted corrupted local OCI content")
	}
}
