// Package runtimepkg imports and verifies platform-specific inference runtimes.
package runtimepkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const Schema = "nexusrun.dev/runtime/v1"
const MetadataFile = "runtime.json"
const maxFiles = 4096
const maxBytes int64 = 4 << 30

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode,omitempty"`
}

type Metadata struct {
	Schema           string   `json:"schema"`
	Engine           string   `json:"engine"`
	Version          string   `json:"version"`
	UpstreamRevision string   `json:"upstream_revision"`
	OS               string   `json:"os"`
	Arch             string   `json:"arch"`
	CPUFeatures      []string `json:"cpu_features,omitempty"`
	Accelerator      string   `json:"accelerator"`
	Libc             string   `json:"libc"`
	HostDependencies []string `json:"host_dependencies,omitempty"`
	Executable       string   `json:"executable"`
	CLIExecutable    string   `json:"cli_executable,omitempty"`
	LibraryPaths     []string `json:"library_paths,omitempty"`
	Protocol         string   `json:"protocol"`
	Features         []string `json:"features,omitempty"`
	Licenses         []string `json:"licenses"`
	Notices          []string `json:"notices,omitempty"`
	BuildProvenance  string   `json:"build_provenance"`
	PayloadDigest    string   `json:"payload_digest"`
	Files            []File   `json:"files"`
}

func (m *Metadata) Validate() error {
	var missing []string
	for k, v := range map[string]string{"schema": m.Schema, "engine": m.Engine, "version": m.Version, "upstream_revision": m.UpstreamRevision, "os": m.OS, "arch": m.Arch, "accelerator": m.Accelerator, "libc": m.Libc, "executable": m.Executable, "protocol": m.Protocol, "build_provenance": m.BuildProvenance, "payload_digest": m.PayloadDigest} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if m.Schema != "" && m.Schema != Schema {
		return fmt.Errorf("unsupported runtime schema %q", m.Schema)
	}
	if len(m.Licenses) == 0 {
		missing = append(missing, "licenses")
	}
	if len(m.Files) == 0 {
		missing = append(missing, "files")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("runtime metadata missing: %s", strings.Join(missing, ", "))
	}
	for label, value := range map[string]string{"engine": m.Engine, "version": m.Version, "os": m.OS, "arch": m.Arch} {
		if value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
			return fmt.Errorf("runtime %s must be one path segment", label)
		}
	}
	if len(strings.TrimPrefix(m.PayloadDigest, "sha256:")) != 64 {
		return errors.New("runtime payload_digest must be SHA-256")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if err := safePath(f.Path); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		if seen[f.Path] {
			return fmt.Errorf("duplicate file metadata %q", f.Path)
		}
		seen[f.Path] = true
		if len(strings.TrimPrefix(f.SHA256, "sha256:")) != 64 || f.Size < 0 {
			return fmt.Errorf("invalid integrity metadata for %q", f.Path)
		}
	}
	if !seen[m.Executable] {
		return fmt.Errorf("server executable %q is not declared in files", m.Executable)
	}
	if m.CLIExecutable != "" && !seen[m.CLIExecutable] {
		return fmt.Errorf("CLI executable %q is not declared in files", m.CLIExecutable)
	}
	for _, p := range append([]string{m.Executable, m.CLIExecutable}, m.LibraryPaths...) {
		if p != "" {
			if err := safePath(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m Metadata) ID() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", m.Engine, m.Version, m.OS, m.Arch, strings.TrimPrefix(m.PayloadDigest, "sha256:"))
}

type Installed struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Metadata Metadata `json:"metadata"`
}

func Root(storeRoot string) string { return filepath.Join(storeRoot, "runtimes") }
func InstallPath(storeRoot string, m Metadata) string {
	return filepath.Join(Root(storeRoot), m.Engine, m.Version, m.OS, m.Arch, strings.TrimPrefix(m.PayloadDigest, "sha256:"))
}

// Import performs bounded extraction into a staging directory, verifies every
// byte, and atomically renames it into the managed store.
func Import(storeRoot, archive string) (*Installed, error) {
	if err := os.MkdirAll(Root(storeRoot), 0o755); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(Root(storeRoot), ".import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var meta Metadata
	foundMeta := false
	seen := map[string]bool{}
	var total int64
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		count++
		if count > maxFiles {
			return nil, errors.New("runtime archive has too many entries")
		}
		if err := safePath(h.Name); err != nil {
			return nil, fmt.Errorf("unsafe runtime entry %q: %w", h.Name, err)
		}
		name := filepath.ToSlash(filepath.Clean(h.Name))
		if seen[name] {
			return nil, fmt.Errorf("duplicate runtime archive entry %q", name)
		}
		seen[name] = true
		if h.Size < 0 || total+h.Size > maxBytes {
			return nil, errors.New("runtime archive exceeds extraction limit")
		}
		total += h.Size
		target := filepath.Join(stage, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			mode := os.FileMode(h.Mode) & 0o755
			if mode == 0 {
				mode = 0o644
			}
			o, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if e != nil {
				return nil, e
			}
			_, ce := io.CopyN(o, tr, h.Size)
			xe := o.Close()
			if ce != nil {
				return nil, ce
			}
			if xe != nil {
				return nil, xe
			}
			if name == MetadataFile {
				b, e := os.ReadFile(target)
				if e != nil {
					return nil, e
				}
				if e = json.Unmarshal(b, &meta); e != nil {
					return nil, fmt.Errorf("parse runtime metadata: %w", e)
				}
				foundMeta = true
			}
		default:
			return nil, fmt.Errorf("runtime archive entry %q has unsupported type %d", name, h.Typeflag)
		}
	}
	if !foundMeta {
		return nil, errors.New("runtime archive has no runtime.json")
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	if err := verifyDir(stage, meta); err != nil {
		return nil, err
	}
	dest := InstallPath(storeRoot, meta)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(dest); err == nil {
		if err := verifyDir(dest, meta); err != nil {
			return nil, fmt.Errorf("installed runtime is corrupt: %w", err)
		}
		return &Installed{meta.ID(), dest, meta}, nil
	}
	if err := os.Rename(stage, dest); err != nil {
		return nil, err
	}
	return &Installed{meta.ID(), dest, meta}, nil
}

func verifyDir(root string, m Metadata) error {
	h := sha256.New()
	for _, f := range m.Files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		info, e := os.Stat(p)
		if e != nil {
			return fmt.Errorf("required file %s: %w", f.Path, e)
		}
		if !info.Mode().IsRegular() || info.Size() != f.Size {
			return fmt.Errorf("file %s size/type mismatch", f.Path)
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		fh := sha256.New()
		_, e = io.Copy(io.MultiWriter(fh, h), in)
		in.Close()
		if e != nil {
			return e
		}
		if hex.EncodeToString(fh.Sum(nil)) != strings.TrimPrefix(f.SHA256, "sha256:") {
			return fmt.Errorf("file %s digest mismatch", f.Path)
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != strings.TrimPrefix(m.PayloadDigest, "sha256:") {
		return errors.New("runtime payload digest mismatch")
	}
	return nil
}

func Verify(storeRoot, id string) (*Installed, error) {
	p, m, e := Resolve(storeRoot, id)
	if e != nil {
		return nil, e
	}
	if e = verifyDir(p, *m); e != nil {
		return nil, e
	}
	return &Installed{id, p, *m}, nil
}

// VerifyDirectory verifies an unpacked runtime before any bundled executable
// is launched.
func VerifyDirectory(path string) (*Metadata, error) {
	b, err := os.ReadFile(filepath.Join(path, MetadataFile))
	if err != nil {
		return nil, err
	}
	var m Metadata
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if err = m.Validate(); err != nil {
		return nil, err
	}
	if err = verifyDir(path, m); err != nil {
		return nil, err
	}
	return &m, nil
}

func Resolve(storeRoot, id string) (string, *Metadata, error) {
	p := filepath.Join(Root(storeRoot), filepath.FromSlash(id))
	rel, e := filepath.Rel(Root(storeRoot), p)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", nil, errors.New("invalid runtime id")
	}
	b, e := os.ReadFile(filepath.Join(p, MetadataFile))
	if e != nil {
		return "", nil, e
	}
	var m Metadata
	if e = json.Unmarshal(b, &m); e != nil {
		return "", nil, e
	}
	if e = m.Validate(); e != nil {
		return "", nil, e
	}
	if m.ID() != id {
		return "", nil, errors.New("runtime identity does not match its path")
	}
	return p, &m, nil
}

func List(storeRoot string) ([]Installed, error) {
	var out []Installed
	root := Root(storeRoot)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if d.Name() != MetadataFile {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return nil
		}
		var m Metadata
		if json.Unmarshal(b, &m) == nil && m.Validate() == nil {
			out = append(out, Installed{m.ID(), filepath.Dir(p), m})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func Compatible(m Metadata, goos, goarch string) error {
	if m.OS != goos || m.Arch != goarch {
		return fmt.Errorf("runtime targets %s/%s, host is %s/%s", m.OS, m.Arch, goos, goarch)
	}
	return nil
}
func HostCompatible(m Metadata) error { return Compatible(m, runtime.GOOS, runtime.GOARCH) }

func Activate(storeRoot, id string) error {
	p, m, e := Resolve(storeRoot, id)
	if e != nil {
		return e
	}
	if e = HostCompatible(*m); e != nil {
		return e
	}
	cur := map[string]any{"id": id, "name": m.Engine, "version": m.Version, "cli": abs(p, m.CLIExecutable), "server": abs(p, m.Executable), "library_paths": absAll(p, m.LibraryPaths), "activated_at": time.Now().UTC()}
	b, _ := json.MarshalIndent(cur, "", "  ")
	dir := filepath.Join(Root(storeRoot), m.Engine)
	if e = os.MkdirAll(dir, 0o755); e != nil {
		return e
	}
	tmp, e := os.CreateTemp(dir, ".current-")
	if e != nil {
		return e
	}
	n := tmp.Name()
	defer os.Remove(n)
	if _, e = tmp.Write(b); e == nil {
		e = tmp.Sync()
	}
	if ce := tmp.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(n, filepath.Join(dir, "current.json"))
}
func abs(root, p string) string {
	if p == "" {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(p))
}
func absAll(root string, ps []string) []string {
	r := make([]string, 0, len(ps))
	for _, p := range ps {
		r = append(r, abs(root, p))
	}
	return r
}
func safePath(p string) error {
	if p == "" || strings.ContainsRune(p, '\x00') || filepath.IsAbs(filepath.FromSlash(p)) {
		return errors.New("path must be relative")
	}
	c := filepath.Clean(filepath.FromSlash(p))
	if c == "." || c == ".." || strings.HasPrefix(c, ".."+string(os.PathSeparator)) {
		return errors.New("path escapes package")
	}
	return nil
}
