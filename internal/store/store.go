// Package store manages NexusRun's on-disk state: a content-addressed
// blob store for model weights, an OCI layout for packaged units, and
// run logs. Model weights are deduplicated by digest so multiple units
// referencing the same model cost one copy on disk.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	return OpenRoot(root)
}

// OpenRoot opens an isolated store without changing process-wide environment.
func OpenRoot(root string) (*Store, error) {
	s := &Store{Root: root}
	// Run logs hold what the models said, so the home is private to this
	// user. MkdirAll leaves an existing directory's mode alone.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	for _, d := range []string{s.BlobsDir(), s.UnitsDir(), s.LogsDir(), s.EvalsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) BlobsDir() string { return filepath.Join(s.Root, "blobs", "sha256") }
func (s *Store) UnitsDir() string { return filepath.Join(s.Root, "units") }
func (s *Store) LogsDir() string  { return filepath.Join(s.Root, "logs") }

// EvalsDir holds saved evaluation reports. They live beside runs rather
// than inside a unit because a score belongs to a (unit, model, host)
// triple, not to the unit alone — the same artifact scores differently on
// different machines, and both numbers are worth keeping.
func (s *Store) EvalsDir() string { return filepath.Join(s.Root, "evals") }

// digestPattern is the only digest shape that may become a blob path. A pin
// comes from a unit's manifest, which is untrusted, and anything else could
// name a file outside the blob store.
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// normalizeDigest accepts a sha256 pin with or without its "sha256:" prefix
// and returns it in the prefixed form, or an error for anything else.
func normalizeDigest(d string) (string, error) {
	if d == "" {
		return "", nil
	}
	if !strings.HasPrefix(d, "sha256:") {
		d = "sha256:" + d
	}
	if !digestPattern.MatchString(d) {
		return "", fmt.Errorf("sha256 pin %q must be 64 lowercase hex digits, optionally prefixed with sha256:", d)
	}
	return d, nil
}

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

	// Quant and Params describe the weights when the source can say so.
	// They matter because quality depends on them: the same agent at Q4 and
	// at Q3 is not the same agent, and a score with no record of which one
	// ran is not a measurement of anything. Ollama states both in its
	// manifest; a bare .gguf file only hints at the quant in its filename.
	Quant  string // Q4_K_M, Q4_0, F16 …
	Params string // 1B, 8B … as the source labels it
}

// Progress reports download progress. It may be nil.
type Progress func(done, total int64)

// Cached resolves a model only when it is already available locally. It is
// the read-only half of Resolve, used by installation preflight so merely
// opening the console never starts a multi-gigabyte download.
func (s *Store) Cached(source, wantDigest string) (*ResolvedModel, bool) {
	switch {
	case strings.HasPrefix(source, "ollama:"):
		m, err := resolveOllama(strings.TrimPrefix(source, "ollama:"))
		return m, err == nil
	case strings.HasPrefix(source, "hf:"), strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		digest, err := normalizeDigest(wantDigest)
		if err != nil {
			return nil, false
		}
		if digest == "" {
			digest = s.cachedDigest(source)
		}
		if digest == "" {
			return nil, false
		}
		info, err := os.Stat(s.BlobPath(digest))
		if err != nil {
			return nil, false
		}
		return &ResolvedModel{Path: s.BlobPath(digest), Digest: digest, Size: info.Size(), Source: source}, true
	default:
		abs, err := filepath.Abs(source)
		if err != nil {
			return nil, false
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return nil, false
		}
		return &ResolvedModel{Path: abs, Size: info.Size(), Source: source, Shared: true}, true
	}
}

// DownloadSize asks the model origin for its byte size without downloading
// the body. A zero size means the origin did not advertise one. Callers can
// fall back to a size declared in nexus.yaml.
func (s *Store) DownloadSize(ctx context.Context, source string) (int64, error) {
	var rawURL string
	isHF := false
	switch {
	case strings.HasPrefix(source, "hf:"):
		isHF = true
		var err error
		rawURL, err = hfResolveURL(strings.TrimPrefix(source, "hf:"))
		if err != nil {
			return 0, err
		}
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		rawURL = source
	default:
		if m, ok := s.Cached(source, ""); ok {
			return m.Size, nil
		}
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	if isHF {
		for k, v := range hfHeaders() {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return 0, fmt.Errorf("inspect %s: %s", source, resp.Status)
	}
	return resp.ContentLength, nil
}

// Resolve turns a manifest model source into a local file path,
// downloading it if necessary. Supported schemes:
//
//	ollama:<name>[:<tag>]              reuse a model already pulled by Ollama (no copy)
//	hf:<org>/<repo>/<file>             Hugging Face, revision "main"
//	hf:<org>/<repo>@<revision>/<file>  Hugging Face, pinned revision or branch
//	https://…                          direct download
//	./path or /path                    local file, used in place
//
// The <file> segment may contain slashes (models with files in subfolders).
// Gated or private Hugging Face repos are reachable by setting HF_TOKEN (or
// HUGGING_FACE_HUB_TOKEN) in the environment.
func (s *Store) Resolve(source, wantDigest string, p Progress) (*ResolvedModel, error) {
	switch {
	case strings.HasPrefix(source, "ollama:"):
		return resolveOllama(strings.TrimPrefix(source, "ollama:"))
	case strings.HasPrefix(source, "hf:"):
		url, err := hfResolveURL(strings.TrimPrefix(source, "hf:"))
		if err != nil {
			return nil, err
		}
		return s.download(url, wantDigest, source, p, hfHeaders())
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return s.download(source, wantDigest, source, p, nil)
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

// hfResolveURL builds a Hugging Face resolve URL from an hf: source spec
// (everything after the "hf:" prefix). The spec is
//
//	<org>/<repo>[@<revision>]/<file...>
//
// The revision defaults to "main". The file segment keeps any slashes, so
// weights stored in a subfolder resolve correctly. The revision segment is
// mandatory in Hugging Face's resolve URL — omitting it (as an earlier
// version did) produces a 404.
func hfResolveURL(spec string) (string, error) {
	parts := strings.SplitN(spec, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("hf source must be hf:<org>/<repo>[@<rev>]/<file>, got %q", "hf:"+spec)
	}
	org, repoRev, file := parts[0], parts[1], parts[2]
	repo, revision, hasRev := strings.Cut(repoRev, "@")
	if !hasRev || revision == "" {
		revision = "main"
	}
	// Path-escape each user segment so a repo or filename cannot inject extra
	// path elements or query strings into the URL.
	return fmt.Sprintf("https://huggingface.co/%s/%s/resolve/%s/%s",
		url.PathEscape(org), url.PathEscape(repo),
		url.PathEscape(revision), hfEscapeFilePath(file)), nil
}

// hfEscapeFilePath escapes each path segment but keeps the slashes between
// them, so "sub/dir/model.gguf" stays a path rather than one escaped blob.
func hfEscapeFilePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// hfHeaders returns the request headers for a Hugging Face download: a
// User-Agent (some CDNs reject an empty one) and a bearer token when the
// environment supplies one, which is what unlocks gated and private repos.
func hfHeaders() map[string]string {
	h := map[string]string{"User-Agent": "nexusrun"}
	for _, k := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "HUGGINGFACE_TOKEN"} {
		if tok := os.Getenv(k); tok != "" {
			h["Authorization"] = "Bearer " + tok
			break
		}
	}
	return h
}

// download fetches a URL into the content-addressed blob store. If the
// digest is known up front and already present, it is a no-op.
func (s *Store) download(rawURL, wantDigest, source string, p Progress, headers map[string]string) (*ResolvedModel, error) {
	wantDigest, err := normalizeDigest(wantDigest)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if wantDigest != "" {
		if info, err := os.Stat(s.BlobPath(wantDigest)); err == nil {
			return &ResolvedModel{Path: s.BlobPath(wantDigest), Digest: wantDigest, Size: info.Size(), Source: source}, nil
		}
	} else if d := s.cachedDigest(source); d != "" {
		// An unpinned source (no sha256 in the manifest) is content-addressed
		// only after the first fetch. Remembering source→digest is what stops
		// a multi-gigabyte model from being re-downloaded on every run.
		if info, err := os.Stat(s.BlobPath(d)); err == nil {
			return &ResolvedModel{Path: s.BlobPath(d), Digest: d, Size: info.Size(), Source: source}, nil
		}
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Gated and private Hugging Face repos fail here without a token;
		// say so rather than leaving the user to guess at a bare 401/403.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("download %s: %s — if this is a gated or private model, set HF_TOKEN", source, resp.Status)
		}
		return nil, fmt.Errorf("download %s: %s", source, resp.Status)
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
	// Remember source→digest so the next unpinned resolve reuses the blob
	// instead of downloading it again. Best-effort: a cache write failure
	// only costs a redundant future download, never correctness.
	if wantDigest == "" {
		s.recordDownload(source, digest)
	}
	return &ResolvedModel{Path: final, Digest: digest, Size: written, Source: source}, nil
}

// downloadsPath is the source→digest index for unpinned downloads.
func (s *Store) downloadsPath() string { return filepath.Join(s.Root, "downloads.json") }

func (s *Store) loadDownloads() map[string]string {
	m := map[string]string{}
	data, err := os.ReadFile(s.downloadsPath())
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	for source, d := range m {
		if !digestPattern.MatchString(d) {
			delete(m, source)
		}
	}
	return m
}

// cachedDigest returns the digest previously recorded for a source, or "".
func (s *Store) cachedDigest(source string) string {
	return s.loadDownloads()[source]
}

// recordDownload maps a source to the digest it resolved to. Read-modify-write
// of a small JSON file; races between concurrent CLI runs at worst drop an
// entry (a redundant download later), never corrupt the blob store, which is
// content-addressed and written by atomic rename.
func (s *Store) recordDownload(source, digest string) {
	m := s.loadDownloads()
	m[source] = digest
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := s.downloadsPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.downloadsPath())
	}
}

// --- Ollama interop -------------------------------------------------------

type ollamaManifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
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

// Ollama model names and tags become path elements, and the digests in its
// manifests become blob file names, so each must be a plain name.
var (
	ollamaNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
	ollamaTagPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// resolveOllama locates the GGUF weights layer for an Ollama model and
// returns its blob path directly — no copy, no second download.
func resolveOllama(ref string) (*ResolvedModel, error) {
	name, tag, ok := strings.Cut(ref, ":")
	if !ok || tag == "" {
		tag = "latest"
	}
	if !ollamaNamePattern.MatchString(name) || !ollamaTagPattern.MatchString(tag) {
		return nil, fmt.Errorf("invalid Ollama model name %q", ref)
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
			if !digestPattern.MatchString(l.Digest) {
				return nil, fmt.Errorf("ollama manifest %s has an invalid layer digest", manifestPath)
			}
			// Ollama stores blobs as sha256-<hex> (dash, not colon).
			blob := filepath.Join(root, "blobs", strings.Replace(l.Digest, ":", "-", 1))
			if _, err := os.Stat(blob); err != nil {
				return nil, fmt.Errorf("ollama model layer missing at %s: %w", blob, err)
			}
			quant, params := ollamaModelInfo(root, m.Config.Digest)
			return &ResolvedModel{
				Path:   blob,
				Digest: l.Digest,
				Size:   l.Size,
				Source: "ollama:" + ref,
				Shared: true,
				Quant:  quant,
				Params: params,
			}, nil
		}
		return nil, fmt.Errorf("ollama model %q has no weights layer", ref)
	}
	return nil, fmt.Errorf("ollama model %q not found; looked in:\n  %s", ref, strings.Join(tried, "\n  "))
}

// ollamaModelInfo reads the quantization and parameter count Ollama records
// in its manifest config blob. Weights borrowed from Ollama are stored under
// their digest, so the filename says nothing — but the metadata is right
// there, and an evaluation is worth much less without it.
//
// Best-effort: a missing or changed config shape costs an empty column, never
// a failed resolve.
func ollamaModelInfo(root, configDigest string) (quant, params string) {
	if !digestPattern.MatchString(configDigest) {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(root, "blobs", strings.Replace(configDigest, ":", "-", 1)))
	if err != nil {
		return "", ""
	}
	var cfg struct {
		FileType  string `json:"file_type"`
		ModelType string `json:"model_type"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", ""
	}
	return cfg.FileType, cfg.ModelType
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
	return os.WriteFile(filepath.Join(s.LogsDir(), r.ID+".json"), data, 0o600)
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
