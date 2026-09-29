package runtimepkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testArchive(t *testing.T, entries map[string]string, mutate func(*Metadata)) string {
	t.Helper()
	files := make([]File, 0, len(entries))
	payload := sha256.New()
	for _, name := range []string{"bin/llama-cli", "bin/llama-server", "lib/libllama.so"} {
		if body, ok := entries[name]; ok {
			h := sha256.Sum256([]byte(body))
			files = append(files, File{Path: name, SHA256: hex.EncodeToString(h[:]), Size: int64(len(body)), Mode: 0755})
			payload.Write([]byte(body))
		}
	}
	m := Metadata{Schema: Schema, Engine: "llama.cpp", Version: "b1", UpstreamRevision: "b1", OS: "linux", Arch: "amd64", Accelerator: "cpu", Libc: "glibc>=2.31", Executable: "bin/llama-server", CLIExecutable: "bin/llama-cli", LibraryPaths: []string{"lib"}, Protocol: "openai-chat/v1", Licenses: []string{"MIT"}, BuildProvenance: "test", PayloadDigest: hex.EncodeToString(payload.Sum(nil)), Files: files}
	if mutate != nil {
		mutate(&m)
	}
	b, _ := json.Marshal(m)
	entries[MetadataFile] = string(b)
	p := filepath.Join(t.TempDir(), "runtime.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		h := &tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if strings.HasPrefix(name, "bin/") {
			h.Mode = 0755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func TestImportVerifyAndDeduplicate(t *testing.T) {
	a := testArchive(t, map[string]string{"bin/llama-cli": "cli", "bin/llama-server": "server", "lib/libllama.so": "lib"}, nil)
	root := t.TempDir()
	r, e := Import(root, a)
	if e != nil {
		t.Fatal(e)
	}
	if e = Activate(root, r.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(root, r.ID); e != nil {
		t.Fatal(e)
	}
	r2, e := Import(root, a)
	if e != nil || r2.ID != r.ID {
		t.Fatalf("idempotent import: %+v %v", r2, e)
	}
}
func TestImportRejectsDigestMismatch(t *testing.T) {
	a := testArchive(t, map[string]string{"bin/llama-cli": "cli", "bin/llama-server": "server"}, func(m *Metadata) { m.Files[0].SHA256 = strings.Repeat("0", 64) })
	if _, e := Import(t.TempDir(), a); e == nil || !strings.Contains(e.Error(), "digest mismatch") {
		t.Fatalf("got %v", e)
	}
}
func TestImportRejectsTraversalAndLinks(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute"} {
		t.Run(name, func(t *testing.T) {
			a := testArchive(t, map[string]string{"bin/llama-cli": "cli", "bin/llama-server": "server", name: "bad"}, nil)
			if _, e := Import(t.TempDir(), a); e == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "x.tgz")
		f, _ := os.Create(p)
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "link", Linkname: "../x", Typeflag: tar.TypeSymlink})
		tw.Close()
		gz.Close()
		f.Close()
		if _, e := Import(t.TempDir(), p); e == nil {
			t.Fatal("symlink accepted")
		}
	})
}
func TestCompatibility(t *testing.T) {
	m := Metadata{OS: "linux", Arch: "arm64"}
	if e := Compatible(m, "linux", "amd64"); e == nil || !strings.Contains(e.Error(), "host is linux/amd64") {
		t.Fatalf("got %v", e)
	}
}
