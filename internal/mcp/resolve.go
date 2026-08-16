package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/verdictlayer/nexusrun/internal/store"
)

// Source kinds a unit may declare.
const (
	KindGitHub = "github"
	KindOCI    = "oci"
	KindLocal  = "file"
	KindNPM    = "npm"
	KindExec   = "exec"
)

// Source is a parsed MCP server location.
type Source struct {
	Kind string
	Raw  string

	// GitHub
	Org, Repo, Ref, Path string

	// Local / exec
	Location string

	// NPM
	Package string
}

// Pinned reports whether the source names an exact revision. A floating
// ref means the server's code can change under a unit that never changed,
// which is the thing unit digests exist to prevent — so it warns at build
// time rather than being silently accepted.
func (s Source) Pinned() bool {
	switch s.Kind {
	case KindGitHub:
		// A 40-character hex ref is a commit; a branch name is not.
		if len(s.Ref) == 40 {
			_, err := hex.DecodeString(s.Ref)
			return err == nil
		}
		return false
	case KindOCI:
		return strings.Contains(s.Raw, "@sha256:")
	case KindNPM:
		return strings.Contains(strings.TrimPrefix(s.Package, "@"), "@")
	default:
		return true // a local path is whatever is on this machine, by definition
	}
}

// ParseSource decodes a source spec.
//
//	github:org/repo#ref/path…    clone at ref, run from path
//	ghcr.io/user/img:tag         OCI artifact
//	file:///absolute/path        use in place
//	npm:@scope/name              install with npm
//	exec:command arg…            run a command already on this machine
func ParseSource(raw string) (Source, error) {
	s := Source{Raw: raw}
	switch {
	case strings.HasPrefix(raw, "github:"):
		s.Kind = KindGitHub
		spec := strings.TrimPrefix(raw, "github:")
		repoPart, refPart, hasRef := strings.Cut(spec, "#")
		org, repo, ok := strings.Cut(repoPart, "/")
		if !ok || org == "" || repo == "" {
			return s, fmt.Errorf("github source must be github:<org>/<repo>[#<ref>/<path>], got %q", raw)
		}
		s.Org, s.Repo = org, repo
		s.Ref = "main"
		if hasRef && refPart != "" {
			ref, sub, hasSub := strings.Cut(refPart, "/")
			s.Ref = ref
			if hasSub {
				s.Path = sub
			}
		}
		return s, nil

	case strings.HasPrefix(raw, "file://"):
		s.Kind = KindLocal
		s.Location = strings.TrimPrefix(raw, "file://")
		if !filepath.IsAbs(s.Location) {
			return s, fmt.Errorf("file source must be an absolute path, got %q", raw)
		}
		return s, nil

	case strings.HasPrefix(raw, "npm:"):
		s.Kind = KindNPM
		s.Package = strings.TrimPrefix(raw, "npm:")
		if s.Package == "" {
			return s, fmt.Errorf("npm source needs a package name, got %q", raw)
		}
		return s, nil

	case strings.HasPrefix(raw, "exec:"):
		s.Kind = KindExec
		s.Location = strings.TrimSpace(strings.TrimPrefix(raw, "exec:"))
		if s.Location == "" {
			return s, fmt.Errorf("exec source needs a command, got %q", raw)
		}
		return s, nil

	case strings.Contains(raw, "/") && (strings.Contains(raw, ":") || strings.Contains(raw, ".")):
		s.Kind = KindOCI
		return s, nil

	default:
		return s, fmt.Errorf(
			"unrecognised MCP source %q — use github:org/repo#ref/path, file:///abs/path, npm:package, exec:command, or an OCI reference",
			raw)
	}
}

// CacheDir is where fetched MCP servers live, shared across units.
func CacheDir(s *store.Store) string { return filepath.Join(s.Root, "mcp") }

// InstallDir is the directory a source resolves to on disk.
func (s Source) InstallDir(st *store.Store) string {
	switch s.Kind {
	case KindLocal:
		return s.Location
	case KindGitHub:
		return filepath.Join(CacheDir(st), "github", s.Org, s.Repo, sanitize(s.Ref))
	case KindNPM:
		return filepath.Join(CacheDir(st), "npm", sanitize(s.Package))
	default:
		// OCI and exec have no tree of their own; key by a digest of the
		// spec so two units naming the same thing share one directory.
		sum := sha256.Sum256([]byte(s.Raw))
		return filepath.Join(CacheDir(st), s.Kind, hex.EncodeToString(sum[:8]))
	}
}

// Installed reports whether the source is already fetched.
func (s Source) Installed(st *store.Store) bool {
	switch s.Kind {
	case KindExec:
		return true // it is whatever is on PATH; Command reports a real failure
	case KindLocal:
		info, err := os.Stat(s.Location)
		return err == nil && info.IsDir()
	default:
		info, err := os.Stat(s.InstallDir(st))
		return err == nil && info.IsDir()
	}
}

// Install fetches the server if it is not already cached.
func (s Source) Install(ctx context.Context, st *store.Store, force bool, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if s.Kind == KindExec {
		return nil
	}
	if s.Kind == KindLocal {
		if !s.Installed(st) {
			return fmt.Errorf("local MCP server not found at %s", s.Location)
		}
		return nil
	}
	dir := s.InstallDir(st)
	if s.Installed(st) && !force {
		logf("%s already installed at %s", s.Raw, dir)
		return nil
	}
	if force {
		_ = os.RemoveAll(dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}

	switch s.Kind {
	case KindGitHub:
		git, err := exec.LookPath("git")
		if err != nil {
			return fmt.Errorf("git is required to fetch %s: %w", s.Raw, err)
		}
		url := fmt.Sprintf("https://github.com/%s/%s.git", s.Org, s.Repo)
		logf("cloning %s at %s", url, s.Ref)
		// A shallow clone of one ref: MCP servers are small and their
		// history is not what is being shipped.
		cmd := exec.CommandContext(ctx, git, "clone", "--depth", "1", "--branch", s.Ref, url, dir)
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			return fmt.Errorf("clone %s: %w\n  %s", url, cerr, tailLines(string(out), 5))
		}
		return nil

	case KindNPM:
		npm, err := exec.LookPath("npm")
		if err != nil {
			return fmt.Errorf(
				"npm is required to install %s, and NexusRun does not bundle a JavaScript runtime — install Node, or use a compiled MCP server: %w",
				s.Raw, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		logf("installing %s with npm", s.Package)
		cmd := exec.CommandContext(ctx, npm, "install", "--no-fund", "--no-audit", "--prefix", dir, s.Package)
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			return fmt.Errorf("npm install %s: %w\n  %s", s.Package, cerr, tailLines(string(out), 8))
		}
		return nil

	case KindOCI:
		return fmt.Errorf(
			"OCI-packaged MCP servers are not implemented yet — use github:, npm:, file:// or exec: for %s", s.Raw)
	}
	return fmt.Errorf("cannot install source kind %q", s.Kind)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
