// Package bundle creates target-specific, offline NexusRun deployments.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/runtimepkg"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

const Schema = "nexusrun.dev/bundle/v1"

type Manifest struct {
	Schema           string   `json:"schema"`
	Unit             string   `json:"unit"`
	UnitDigest       string   `json:"unit_digest"`
	RuntimeID        string   `json:"runtime_id"`
	RuntimeDigest    string   `json:"runtime_digest"`
	Target           string   `json:"target"`
	NexusSHA256      string   `json:"nexus_sha256"`
	Created          string   `json:"created"`
	HostDependencies []string `json:"host_dependencies,omitempty"`
}
type Options struct{ Ref, Target, RuntimeID, NexusPath, Output string }

func Export(ctx context.Context, s *store.Store, o Options) (*Manifest, error) {
	if o.Output == "" || o.NexusPath == "" {
		return nil, fmt.Errorf("output and target NexusRun binary are required")
	}
	parts := strings.Split(o.Target, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("target must be os/arch")
	}
	if err := validateNexusBinary(o.NexusPath, parts[0], parts[1]); err != nil {
		return nil, err
	}
	rp, rm, err := runtimepkg.Resolve(s.Root, o.RuntimeID)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime: %w", err)
	}
	if err = runtimepkg.Compatible(*rm, parts[0], parts[1]); err != nil {
		return nil, err
	}
	if err = unit.ValidateOfflineAgent(ctx, s, o.Ref, o.Target); err != nil {
		return nil, err
	}
	m, _, err := unit.Resolve(ctx, s, o.Ref)
	if err != nil {
		return nil, err
	}
	ud, err := unit.Digest(ctx, s, o.Ref)
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp("", "nexus-bundle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	for _, d := range []string{"bin", "runtime", "agent", "licenses"} {
		if err = os.MkdirAll(filepath.Join(stage, d), 0o755); err != nil {
			return nil, err
		}
	}
	if err = copyFile(o.NexusPath, filepath.Join(stage, "bin", "nexus"), 0o755); err != nil {
		return nil, err
	}
	if err = copyTree(rp, filepath.Join(stage, "runtime")); err != nil {
		return nil, err
	}
	agentPath := filepath.Join(stage, "agent", m.Name+".nx")
	if err = unit.ExportFile(ctx, s, o.Ref, agentPath); err != nil {
		return nil, err
	}
	for _, n := range rm.Notices {
		name := filepath.Base(n)
		src := filepath.Join(rp, filepath.FromSlash(n))
		if _, e := os.Stat(src); e == nil {
			_ = copyFile(src, filepath.Join(stage, "licenses", name), 0o644)
		}
	}
	nh, err := fileSHA(filepath.Join(stage, "bin", "nexus"))
	if err != nil {
		return nil, err
	}
	bm := &Manifest{Schema: Schema, Unit: m.Ref(), UnitDigest: ud, RuntimeID: rm.ID(), RuntimeDigest: rm.PayloadDigest, Target: o.Target, NexusSHA256: nh, Created: time.Now().UTC().Format(time.RFC3339), HostDependencies: rm.HostDependencies}
	b, _ := json.MarshalIndent(bm, "", "  ")
	if err = os.WriteFile(filepath.Join(stage, "bundle.json"), b, 0o644); err != nil {
		return nil, err
	}
	launcher := launcherScript(m.Ref(), rm)
	if err = os.WriteFile(filepath.Join(stage, "run"), []byte(launcher), 0o755); err != nil {
		return nil, err
	}
	if err = writeTGZ(stage, o.Output); err != nil {
		return nil, err
	}
	return bm, nil
}

func validateNexusBinary(path, targetOS, targetArch string) error {
	if targetOS != "linux" {
		return fmt.Errorf("deployment bundles currently support linux targets, got %s", targetOS)
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("target NexusRun binary is not Linux ELF: %w", err)
	}
	defer f.Close()
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[targetArch]
	if want == elf.EM_NONE {
		return fmt.Errorf("unsupported Linux bundle architecture %s", targetArch)
	}
	if f.Machine != want {
		return fmt.Errorf("target NexusRun binary architecture is %s, expected %s", f.Machine, targetArch)
	}
	return nil
}

func launcherScript(ref string, m *runtimepkg.Metadata) string {
	lib := ""
	if len(m.LibraryPaths) > 0 {
		var p []string
		for _, v := range m.LibraryPaths {
			p = append(p, "$BASE/runtime/"+v)
		}
		lib = "export LD_LIBRARY_PATH=\"" + strings.Join(p, ":") + "${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\"\n"
	}
	return "#!/bin/sh\nset -eu\nBASE=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\nexport NEXUSRUN_HOME=\"${NEXUSRUN_HOME:-$BASE/state}\"\nmkdir -p \"$NEXUSRUN_HOME\"\n\"$BASE/bin/nexus\" runtime verify-dir \"$BASE/runtime\" >/dev/null\nexport NEXUSRUN_LLAMA_SERVER=\"$BASE/runtime/" + m.Executable + "\"\nexport NEXUSRUN_LLAMA_CLI=\"$BASE/runtime/" + m.CLIExecutable + "\"\n" + lib + "\"$BASE/bin/nexus\" import \"$BASE/agent/" + strings.Split(ref, ":")[0] + ".nx\" >/dev/null\nexec \"$BASE/bin/nexus\" run --no-daemon --backend llama.cpp/server \"" + ref + "\" \"$@\"\n"
}
func copyFile(src, dst string, mode os.FileMode) error {
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	ce := out.Close()
	if e != nil {
		return e
	}
	return ce
}
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("runtime contains unsupported file %s", rel)
		}
		return copyFile(p, to, info.Mode()&0o755)
	})
}
func fileSHA(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func writeTGZ(root, out string) error {
	f, e := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if e != nil {
		return e
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	e = filepath.Walk(root, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		h, e := tar.FileInfoHeader(i, "")
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(rel)
		if e = tw.WriteHeader(h); e != nil {
			return e
		}
		if i.Mode().IsRegular() {
			in, e := os.Open(p)
			if e != nil {
				return e
			}
			_, e = io.Copy(tw, in)
			in.Close()
			return e
		}
		return nil
	})
	if ce := tw.Close(); e == nil {
		e = ce
	}
	if ce := gz.Close(); e == nil {
		e = ce
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	return e
}
