package unit

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitRefTag(t *testing.T) {
	tests := []struct {
		in, name, tag string
		ok            bool
	}{
		{"ghcr.io/lance/agent:0.1.0", "ghcr.io/lance/agent", "0.1.0", true},
		{"localhost:5000/agent:v2", "localhost:5000/agent", "v2", true},
		{"ghcr.io/lance/agent", "ghcr.io/lance/agent", "", false},
		// A port with no tag must not be mistaken for a tag.
		{"localhost:5000/agent", "localhost:5000/agent", "", false},
	}
	for _, tt := range tests {
		name, tag, ok := splitRefTag(tt.in)
		if name != tt.name || tag != tt.tag || ok != tt.ok {
			t.Errorf("splitRefTag(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, name, tag, ok, tt.name, tt.tag, tt.ok)
		}
	}
}

func TestIsIgnored(t *testing.T) {
	ignored := []string{".git/config", "node_modules/x/y.js", "__pycache__/m.pyc", "a/.venv/lib", "old.nx"}
	kept := []string{"nexus.yaml", "src/main.py", "prompts/system.txt"}
	for _, p := range ignored {
		if !isIgnored(p) {
			t.Errorf("isIgnored(%q) = false, want true", p)
		}
	}
	for _, p := range kept {
		if isIgnored(p) {
			t.Errorf("isIgnored(%q) = true, want false", p)
		}
	}
}

func TestTarGzRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "nexus.yaml"), "name: x")
	write(t, filepath.Join(src, "sub", "main.py"), "print('hi')")
	write(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main")

	data, err := tarGzDir(src)
	if err != nil {
		t.Fatalf("tarGzDir: %v", err)
	}
	dst := t.TempDir()
	if err := untarGz(bytes.NewReader(data), dst); err != nil {
		t.Fatalf("untarGz: %v", err)
	}
	if got := read(t, filepath.Join(dst, "sub", "main.py")); got != "print('hi')" {
		t.Errorf("main.py = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "HEAD")); err == nil {
		t.Error(".git was packed, want it ignored")
	}
}

// A malicious artifact must not be able to write outside the unpack
// directory via "../" entries in tar headers.
func TestUntarRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escaped.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()

	dest := t.TempDir()
	err := untar(bytes.NewReader(buf.Bytes()), dest)
	if err == nil {
		t.Fatal("untar accepted a path-traversal entry, want error")
	}
	if !strings.Contains(err.Error(), "outside destination") {
		t.Errorf("error = %v, want it to mention refusing to extract outside destination", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.txt")); statErr == nil {
		t.Fatal("file was written outside the destination directory")
	}
}

func TestTarGzIsReproducible(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "nexus.yaml"), "name: x")

	first, err := tarGzDir(src)
	if err != nil {
		t.Fatal(err)
	}
	// Touch the file so only its mtime changes.
	if err := os.Chtimes(filepath.Join(src, "nexus.yaml"), zeroPlus(), zeroPlus()); err != nil {
		t.Fatal(err)
	}
	second, err := tarGzDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("tarGzDir output changed when only mtime changed; builds are not reproducible")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func zeroPlus() time.Time { return time.Now().Add(-time.Hour) }
