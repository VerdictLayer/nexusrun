package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestResolveLocalPathUsedInPlace(t *testing.T) {
	s := testStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(path, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.Resolve(path, "", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != path {
		t.Errorf("Path = %q, want the original %q — local models must not be copied", got.Path, path)
	}
	if !got.Shared {
		t.Error("Shared = false; a file owned by the user must not be treated as store-owned")
	}
}

func TestResolveMissingLocalPathNamesTheFile(t *testing.T) {
	s := testStore(t)
	_, err := s.Resolve(filepath.Join(t.TempDir(), "absent.gguf"), "", nil)
	if err == nil {
		t.Fatal("expected an error for a missing model file")
	}
	if !strings.Contains(err.Error(), "absent.gguf") {
		t.Errorf("error must name the missing path, got: %v", err)
	}
}

func TestResolveRejectsMalformedHFSource(t *testing.T) {
	s := testStore(t)
	_, err := s.Resolve("hf:org/repo", "", nil)
	if err == nil {
		t.Fatal("expected an error for an incomplete hf: source")
	}
	if !strings.Contains(err.Error(), "hf:<org>/<repo>") {
		t.Errorf("error must show the expected form, got: %v", err)
	}
}

// The resolve URL must carry a revision segment; omitting it (as an earlier
// version did) produced a 404 on every Hugging Face download.
func TestHFResolveURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"unsloth/SmolLM2-135M-Instruct-GGUF/SmolLM2-135M-Instruct-Q2_K.gguf",
			"https://huggingface.co/unsloth/SmolLM2-135M-Instruct-GGUF/resolve/main/SmolLM2-135M-Instruct-Q2_K.gguf",
		},
		{
			// Pinned revision or branch after the repo.
			"Qwen/Qwen2.5-0.5B-Instruct-GGUF@v1.0/qwen2.5-0.5b-instruct-q4_0.gguf",
			"https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/v1.0/qwen2.5-0.5b-instruct-q4_0.gguf",
		},
		{
			// A file kept in a subfolder keeps its slashes.
			"org/repo/models/q4/model.gguf",
			"https://huggingface.co/org/repo/resolve/main/models/q4/model.gguf",
		},
	}
	for _, c := range cases {
		got, err := hfResolveURL(c.in)
		if err != nil {
			t.Errorf("hfResolveURL(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("hfResolveURL(%q)\n  got  %s\n  want %s", c.in, got, c.want)
		}
	}
}

func TestHFResolveURLRejectsIncomplete(t *testing.T) {
	for _, bad := range []string{"org", "org/repo", "org//file", "/repo/file", "org/repo/"} {
		if _, err := hfResolveURL(bad); err == nil {
			t.Errorf("expected error for incomplete spec %q", bad)
		}
	}
}

func TestHFHeadersToken(t *testing.T) {
	// No token → User-Agent only, no Authorization.
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "")
	t.Setenv("HUGGINGFACE_TOKEN", "")
	if _, ok := hfHeaders()["Authorization"]; ok {
		t.Error("Authorization set with no token in the environment")
	}
	// Token present → bearer auth, so gated repos are reachable.
	t.Setenv("HF_TOKEN", "hf_secret")
	if got := hfHeaders()["Authorization"]; got != "Bearer hf_secret" {
		t.Errorf("Authorization = %q, want Bearer hf_secret", got)
	}
}

// A manifest's sha256 pin is an integrity control: a mismatch must fail
// loudly rather than execute unexpected weights.
func TestDownloadVerifiesDigest(t *testing.T) {
	s := testStore(t)
	body := []byte("not the weights you asked for")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	wrong := "sha256:" + strings.Repeat("00", 32)
	_, err := s.Resolve(srv.URL, wrong, nil)
	if err == nil {
		t.Fatal("expected an integrity failure")
	}
	if !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("error should name the integrity failure, got: %v", err)
	}

	// The rejected bytes must not be left behind as a usable blob.
	entries, _ := os.ReadDir(s.BlobsDir())
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".download-") {
			t.Errorf("failed download left a blob behind: %s", e.Name())
		}
	}
}

func TestDownloadStoresByDigestAndReuses(t *testing.T) {
	s := testStore(t)
	body := []byte("the actual weights")
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(body)
	}))
	defer srv.Close()

	got, err := s.Resolve(srv.URL, digest, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Digest != digest {
		t.Errorf("Digest = %q, want %q", got.Digest, digest)
	}
	if got.Path != s.BlobPath(digest) {
		t.Errorf("Path = %q, want the content-addressed location %q", got.Path, s.BlobPath(digest))
	}

	// Second resolve of a pinned model must hit the cache, not the network.
	if _, err := s.Resolve(srv.URL, digest, nil); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if hits != 1 {
		t.Errorf("server hit %d times; a pinned model already on disk must not re-download", hits)
	}
}

// Reusing Ollama's blobs without copying them is a headline feature, and
// it depends on Ollama's exact manifest and blob-naming layout.
func TestResolveOllamaReadsBlobInPlace(t *testing.T) {
	s := testStore(t)
	root := t.TempDir()
	t.Setenv("OLLAMA_MODELS", root)

	weights := []byte("gguf weights")
	sum := sha256.Sum256(weights)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	// Ollama stores blobs with a dash, not a colon.
	blobPath := filepath.Join(root, "blobs", strings.Replace(digest, ":", "-", 1))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, weights, 0o644); err != nil {
		t.Fatal(err)
	}

	// Unqualified names live under the library namespace.
	manifestPath := filepath.Join(root, "manifests", "registry.ollama.ai", "library", "phi3", "latest")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	man := map[string]any{"layers": []map[string]any{
		{"mediaType": "application/vnd.ollama.image.template", "digest": "sha256:dead", "size": 1},
		{"mediaType": "application/vnd.ollama.image.model", "digest": digest, "size": len(weights)},
	}}
	data, _ := json.Marshal(man)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.Resolve("ollama:phi3", "", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != blobPath {
		t.Errorf("Path = %q, want Ollama's blob %q — the point is not to copy it", got.Path, blobPath)
	}
	if !got.Shared {
		t.Error("Shared = false; Ollama owns this file and it must not be deleted or moved")
	}

	// ListOllamaModels deliberately scans every known Ollama root, not
	// just OLLAMA_MODELS, so it may also report the host's real models.
	// Assert only that the fixture is discovered.
	var found bool
	for _, m := range ListOllamaModels() {
		if m == "phi3:latest" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListOllamaModels() did not include the fixture model phi3:latest")
	}
}

func TestResolveOllamaMissingModelListsWhereItLooked(t *testing.T) {
	s := testStore(t)
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	_, err := s.Resolve("ollama:nope", "", nil)
	if err == nil {
		t.Fatal("expected an error for a model Ollama does not have")
	}
	if !strings.Contains(err.Error(), "looked in") {
		t.Errorf("error must say where it searched, got: %v", err)
	}
}

func TestRunsRoundTripNewestFirst(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"20260101T000000Z-a", "20260102T000000Z-b", "20260103T000000Z-c"} {
		if err := s.SaveRun(&RunRecord{
			ID: id, Unit: "u:1", Started: time.Now(), Device: "cpu", Backend: "llama.cpp",
		}); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
	}
	runs, err := s.Runs()
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3", len(runs))
	}
	if runs[0].ID != "20260103T000000Z-c" {
		t.Errorf("Runs()[0] = %q, want the newest record", runs[0].ID)
	}
}
