// Package runtimeinstall owns the small, pinned catalog used by the guided
// installer. Runtimes are installed under the NexusRun store, never into a
// system directory, so the flow needs neither sudo nor a package manager.
package runtimeinstall

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

	"github.com/verdictlayer/nexusrun/internal/store"
)

type Release struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	URL       string   `json:"url"`
	SHA256    string   `json:"sha256"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	Devices   []string `json:"devices"`
	Tested    string   `json:"tested_configuration"`
}

// Recommended returns a release NexusRun has tested for the current target.
// Deliberately absent combinations are unsupported instead of receiving an
// unverified best guess.
func Recommended(goos, goarch string) (Release, bool) {
	for _, r := range catalog {
		if r.OS == goos && r.Arch == goarch {
			return r, true
		}
	}
	return Release{}, false
}

var catalog = []Release{
	{Name: "llama.cpp", Version: "b10695", OS: "linux", Arch: "amd64",
		URL:     "https://github.com/ggml-org/llama.cpp/releases/download/b10695/llama-b10695-bin-ubuntu-x64.tar.gz",
		SHA256:  "4262a9df95bfda0683c9d34b4e7c8550cc323402ae6e6086c7f0ba0a6263ef5f",
		Devices: []string{"cpu"}, Tested: "llama.cpp/CPU with context 4096"},
	{Name: "llama.cpp", Version: "b9637", OS: "linux", Arch: "arm64",
		URL:     "https://github.com/ggml-org/llama.cpp/releases/download/b9637/llama-b9637-bin-ubuntu-arm64.tar.gz",
		SHA256:  "211d9e9ee738698beb7ca271be82661ae2b5da3fbb489cf7d9e4e6ed601be106",
		Devices: []string{"cpu"}, Tested: "llama.cpp/CPU with context 4096"},
	{Name: "llama.cpp", Version: "b9637", OS: "darwin", Arch: "arm64",
		URL:     "https://github.com/ggml-org/llama.cpp/releases/download/b9637/llama-b9637-bin-macos-arm64.tar.gz",
		SHA256:  "72a93f3e68c31de3e438d462669aad1fcdb423b995e9c41033cc7d27a9a3ac69",
		Devices: []string{"gpu", "cpu"}, Tested: "llama.cpp/Metal with CPU fallback and context 4096"},
	{Name: "llama.cpp", Version: "b9637", OS: "darwin", Arch: "amd64",
		URL:     "https://github.com/ggml-org/llama.cpp/releases/download/b9637/llama-b9637-bin-macos-x64.tar.gz",
		SHA256:  "71743f8db0958e7c266cceb7add7b16aa418a964667e471094aa6ae65b9c8298",
		Devices: []string{"cpu"}, Tested: "llama.cpp/CPU with context 4096"},
}

func HostRecommendation() (Release, bool) { return Recommended(runtime.GOOS, runtime.GOARCH) }

func (r Release) InspectSize(ctx context.Context) int64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, r.URL, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return 0
	}
	return resp.ContentLength
}

type current struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	CLI     string `json:"cli"`
	Server  string `json:"server,omitempty"`
}

func CurrentPath(root string) string {
	return filepath.Join(root, "runtimes", "llama.cpp", "current.json")
}

// Install downloads, verifies and extracts a release. progress receives
// bytes downloaded and the advertised total.
func Install(ctx context.Context, s *store.Store, r Release, progress store.Progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s %s: %s", r.Name, r.Version, resp.Status)
	}
	tmp, err := os.CreateTemp("", "nexus-runtime-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var done int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := tmp.Write(buf[:n]); err != nil {
				tmp.Close()
				return err
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(done, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return rerr
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if r.SHA256 != "" && got != strings.TrimPrefix(r.SHA256, "sha256:") {
		return fmt.Errorf("runtime integrity check failed: expected %s, got %s", r.SHA256, got)
	}
	dest := filepath.Join(s.Root, "runtimes", r.Name, r.Version)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := extractTGZ(tmp.Name(), dest); err != nil {
		return err
	}
	cli, err := findBinary(dest, "llama-cli")
	if err != nil {
		return err
	}
	server, _ := findBinary(dest, "llama-server")
	meta := current{Name: r.Name, Version: r.Version, CLI: cli, Server: server}
	data, _ := json.MarshalIndent(meta, "", "  ")
	path := CurrentPath(s.Root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func extractTGZ(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(h.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("runtime archive entry %q escapes destination", h.Name)
		}
		target := filepath.Join(dest, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(h.Mode) & 0o777
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			link := filepath.Clean(filepath.FromSlash(h.Linkname))
			if filepath.IsAbs(link) || link == ".." || strings.HasPrefix(link, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("runtime archive link %q escapes destination", h.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("runtime archive entry %q has unsupported type", h.Name)
		}
	}
}

func findBinary(root, name string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("%s was not present in the runtime archive", name)
	}
	return found, nil
}
