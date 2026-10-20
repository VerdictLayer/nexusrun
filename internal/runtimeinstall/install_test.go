package runtimeinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/store"
)

func archive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestInstallRefusesAnUnpinnedRelease(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	err := Install(context.Background(), &store.Store{Root: t.TempDir()}, Release{Name: "llama.cpp", Version: "x", URL: srv.URL}, nil)
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("want a refusal naming the missing pin, got %v", err)
	}
	if hit {
		t.Error("an unpinned release must be refused before anything is downloaded")
	}
}

func TestInstallRejectsAWrongDigestAndInstallsARightOne(t *testing.T) {
	data := archive(t, map[string]string{"bin/llama-cli": "#!/bin/sh\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	sum := sha256.Sum256(data)

	root := t.TempDir()
	bad := Release{Name: "llama.cpp", Version: "t1", URL: srv.URL, SHA256: strings.Repeat("0", 64)}
	if err := Install(context.Background(), &store.Store{Root: root}, bad, nil); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("a mismatched digest must fail, got %v", err)
	}
	good := Release{Name: "llama.cpp", Version: "t1", URL: srv.URL, SHA256: hex.EncodeToString(sum[:])}
	if err := Install(context.Background(), &store.Store{Root: root}, good, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "runtimes", "llama.cpp", "t1", "bin", "llama-cli")); err != nil {
		t.Error(err)
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.tgz")
	os.WriteFile(p, archive(t, map[string]string{"../escape": "x"}), 0o600)
	if err := extractTGZ(p, t.TempDir()); err == nil {
		t.Fatal("an entry outside the destination must be rejected")
	}
}
