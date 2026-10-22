package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckModelFileGrantsOnlyModels(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, p := range []string{write("model.gguf", "GGUF\x03\x00\x00\x00"), write("blob", "GGUF rest"), write("model.onnx", "\x08\x07")} {
		if err := CheckModelFile(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
	for _, p := range []string{write("id_rsa", "-----BEGIN OPENSSH PRIVATE KEY-----"), write("tiny.gguf", "GG"), dir} {
		if err := CheckModelFile(p); err == nil {
			t.Errorf("%s granted as a model", filepath.Base(p))
		}
	}
}
