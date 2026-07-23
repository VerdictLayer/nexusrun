// Package store manages NexusRun's on-disk state: a content-addressed
// blob store for model weights, an OCI layout for packaged units, and
// run logs. Model weights are deduplicated by digest so multiple units
// referencing the same model cost one copy on disk.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Store is a handle to the NexusRun home directory.
type Store struct {
	Root string
}

// Open returns the store rooted at $NEXUSRUN_HOME, or the per-user
// default, creating the directory tree if needed.
func Open() (*Store, error) {
	root := os.Getenv("NEXUSRUN_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".nexusrun")
	}
	s := &Store{Root: root}
	for _, d := range []string{s.BlobsDir(), s.UnitsDir(), s.LogsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) BlobsDir() string { return filepath.Join(s.Root, "blobs", "sha256") }
func (s *Store) UnitsDir() string { return filepath.Join(s.Root, "units") }
func (s *Store) LogsDir() string  { return filepath.Join(s.Root, "logs") }

// BlobPath returns the on-disk path for a sha256 hex digest.
func (s *Store) BlobPath(digest string) string {
	return filepath.Join(s.BlobsDir(), strings.TrimPrefix(digest, "sha256:"))
}

// ResolvedModel is a model located on local disk and ready to execute.
type ResolvedModel struct {
	Path   string // absolute path to the weights file
	Digest string // sha256:… when known
	Size   int64
	Source string // where it came from, for logging
	Shared bool   // true when the file is owned by another tool (e.g. Ollama)
}

// Progress reports download progress. It may be nil.
type Progress func(done, total int64)

// Resolve turns a manifest model source into a local file path,
// downloading it if necessary. Supported schemes:
//
//	ollama:<name>[:<tag>]        reuse a model already pulled by Ollama (no copy)
//	hf:<org>/<repo>/<file>       Hugging Face resolve endpoint
//	https://…                    direct download
//	./path or /path              local file, used in place
func (s *Store) Resolve(source, wantDigest string, p Progress) (*ResolvedModel, error) {
	switch {
	case strings.HasPrefix(source, "ollama:"):
		return resolveOllama(strings.TrimPrefix(source, "ollama:"))
	case strings.HasPrefix(source, "hf:"):
		spec := strings.TrimPrefix(source, "hf:")
		parts := strings.SplitN(spec, "/", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("hf source must be hf:<org>/<repo>/<file>, got %q", source)
		}
		url := fmt.Sprintf("https://huggingface.co/%s/%s/resolve/%s?download=true", parts[0], parts[1], parts[2])
		return s.download(url, wantDigest, source, p)
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return s.download(source, wantDigest, source, p)
	default:
		abs, err := filepath.Abs(source)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("model not found at %s: %w", abs, err)
		}
		return &ResolvedModel{Path: abs, Size: info.Size(), Source: source, Shared: true}, nil
	}
}

// download fetches a URL into the content-addressed blob store. If the
// digest is known up front and already present, it is a no-op.
func (s *Store) download(url, wantDigest, source string, p Progress) (*ResolvedModel, error) {
	if wantDigest != "" {
		if info, err := os.Stat(s.BlobPath(wantDigest)); err == nil {
			return &ResolvedModel{Path: s.BlobPath(wantDigest), Digest: wantDigest, Size: info.Size(), Source: source}, nil
		}
	}

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(s.BlobsDir(), ".download-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	var written int64
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				return nil, werr
			}
			h.Write(buf[:n])
			written += int64(n)
			if p != nil {
				p(written, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return nil, rerr
		}
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if wantDigest != "" && digest != wantDigest {
		return nil, fmt.Errorf("integrity check failed for %s:\n  expected %s\n  got      %s", source, wantDigest, digest)
	}

	final := s.BlobPath(digest)
	if err := os.Rename(tmpName, final); err != nil {
		return nil, err
	}
	return &ResolvedModel{Path: final, Digest: digest, Size: written, Source: source}, nil
}

// --- Ollama interop -------------------------------------------------------

type ollamaManifest struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// ollamaRoots returns candidate Ollama model directories, covering both
// per-user installs and the system service install.
func ollamaRoots() []string {
	var roots []string
	if v := os.Getenv("OLLAMA_MODELS"); v != "" {
		roots = append(roots, v)
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".ollama", "models"))
	}
	switch runtime.GOOS {
	case "linux":
		roots = append(roots, "/usr/share/ollama/.ollama/models", "/var/lib/ollama/models")
	case "darwin":
		roots = append(roots, "/usr/local/share/ollama/.ollama/models")
	}
	return roots
}

// resolveOllama locates the GGUF weights layer for an Ollama model and
// returns its blob path directly — no copy, no second download.
func resolveOllama(ref string) (*ResolvedModel, error) {
	name, tag, ok := strings.Cut(ref, ":")
	if !ok || tag == "" {
		tag = "latest"
	}
	// Unqualified names live under the library namespace.
	repo := name
	if !strings.Contains(name, "/") {
		repo = "library/" + name
	}

	var tried []string
	for _, root := range ollamaRoots() {
		manifestPath := filepath.Join(root, "manifests", "registry.ollama.ai", filepath.FromSlash(repo), tag)
		tried = append(tried, manifestPath)
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var m ollamaManifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parse ollama manifest %s: %w", manifestPath, err)
		}
		for _, l := range m.Layers {
			if l.MediaType != "application/vnd.ollama.image.model" {
				continue
			}
			// Ollama stores blobs as sha256-<hex> (dash, not colon).
			blob := filepath.Join(root, "blobs", strings.Replace(l.Digest, ":", "-", 1))
			if _, err := os.Stat(blob); err != nil {
				return nil, fmt.Errorf("ollama model layer missing at %s: %w", blob, err)
			}
			return &ResolvedModel{
				Path:   blob,
				Digest: l.Digest,
				Size:   l.Size,
				Source: "ollama:" + ref,
				Shared: true,
			}, nil
		}
		return nil, fmt.Errorf("ollama model %q has no weights layer", ref)
	}
	return nil, fmt.Errorf("ollama model %q not found; looked in:\n  %s", ref, strings.Join(tried, "\n  "))
}

// ListOllamaModels enumerates locally available Ollama models, so
// `nexus models` can show what is runnable without any download.
func ListOllamaModels() []string {
	seen := map[string]bool{}
	var out []string
	for _, root := range ollamaRoots() {
		base := filepath.Join(root, "manifests", "registry.ollama.ai")
		filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // missing roots are expected
			}
			rel, rerr := filepath.Rel(base, path)
			if rerr != nil {
				return nil
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) < 2 {
				return nil
			}
			tag := parts[len(parts)-1]
			name := strings.Join(parts[:len(parts)-1], "/")
			name = strings.TrimPrefix(name, "library/")
			ref := name + ":" + tag
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
			return nil
		})
	}
	return out
}

// --- run records ----------------------------------------------------------

// RunRecord is the metadata written for every unit execution, consumed
// by `nexus logs` and the web console.
type RunRecord struct {
	ID        string    `json:"id"`
	Unit      string    `json:"unit"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitempty"`
	Device    string    `json:"device"`
	Backend   string    `json:"backend"`
	ExitCode  int       `json:"exit_code"`
	TokensOut int       `json:"tokens_out,omitempty"`
	TokPerSec float64   `json:"tokens_per_sec,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// SaveRun persists a run record as JSON under the logs directory.
func (s *Store) SaveRun(r *RunRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.LogsDir(), r.ID+".json"), data, 0o644)
}

// LogPath returns the stdout/stderr capture path for a run.
func (s *Store) LogPath(runID string) string {
	return filepath.Join(s.LogsDir(), runID+".log")
}

// Runs returns all run records, newest first.
func (s *Store) Runs() ([]*RunRecord, error) {
	entries, err := os.ReadDir(s.LogsDir())
	if err != nil {
		return nil, err
	}
	var out []*RunRecord
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.LogsDir(), e.Name()))
		if err != nil {
			continue
		}
		var r RunRecord
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		out = append(out, &r)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
