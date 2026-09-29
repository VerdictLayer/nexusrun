package unit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/store"
)

// ValidateOffline proves that ref can be installed on a declared target
// without contacting a registry or relying on host programs. Secrets are the
// deliberate exception: declarations travel, values must never enter a unit.
func ValidateOffline(ctx context.Context, s *store.Store, ref, target string) error {
	return validateOffline(ctx, s, ref, target, true)
}

// ValidateOfflineAgent checks the portable agent portion of a deployment
// bundle. The caller supplies and independently verifies the runtime package.
func ValidateOfflineAgent(ctx context.Context, s *store.Store, ref, target string) error {
	return validateOffline(ctx, s, ref, target, false)
}

func validateOffline(ctx context.Context, s *store.Store, ref, target string, requireEmbeddedRuntime bool) error {
	m, om, err := Resolve(ctx, s, ref)
	if err != nil {
		return err
	}
	var chosen *manifest.InstallTarget
	for i := range m.Installation.Targets {
		t := &m.Installation.Targets[i]
		label := t.OS + "/" + t.Arch
		if t.Device != "" {
			label += "/" + t.Device
		}
		if target == "" || target == label || target == t.OS+"/"+t.Arch {
			if chosen != nil {
				return fmt.Errorf("offline export target %q matches multiple declarations; choose a full os/arch/device target", target)
			}
			chosen = t
		}
	}
	if chosen == nil {
		return fmt.Errorf("offline export target %q is not declared in installation.targets", target)
	}
	var problems []string
	if om.Annotations[AnnotationSealed] != "true" {
		problems = append(problems, "model weights are linked, not sealed (rebuild with --seal)")
	}
	want := 0
	for _, mod := range m.Models {
		if mod.Auto() {
			want += len(mod.Candidates)
		} else {
			want++
		}
	}
	have := 0
	for _, l := range om.Layers {
		if l.MediaType == MediaTypeModelGGUF {
			have++
		}
	}
	if have < want {
		problems = append(problems, fmt.Sprintf("artifact has %d of %d required model layer(s)", have, want))
	}
	tmp, err := os.MkdirTemp("", "nexus-offline-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err = Unpack(ctx, s, ref, tmp); err != nil {
		return err
	}
	if r := m.Installation.Runtime; r == nil && requireEmbeddedRuntime {
		problems = append(problems, "no pinned installation.runtime bundle is declared")
	} else if r != nil && requireEmbeddedRuntime {
		path := filepath.Join(tmp, filepath.FromSlash(r.Path))
		info, e := os.Stat(path)
		if e != nil {
			problems = append(problems, fmt.Sprintf("runtime %s %s is missing at %s", r.Name, r.Version, r.Path))
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			problems = append(problems, "installation.runtime.path is not a file or directory")
		}
	}
	for _, name := range m.MCPNames() {
		problems = append(problems, fmt.Sprintf("MCP server %s (%s) is externally resolved; bundle it as an inline tool for offline export", name, m.MCPServers[name].Source))
	}
	if !requireEmbeddedRuntime {
		if m.Entrypoint.Type != "chat" {
			problems = append(problems, "the first deployment-bundle profile supports chat entrypoints only")
		}
		if len(m.Tools) > 0 {
			problems = append(problems, "the first deployment-bundle profile does not yet support packaged tools")
		}
	}
	checkCommand := func(label string, argv []string) {
		if len(argv) == 0 {
			return
		}
		cmd := argv[0]
		switch strings.ToLower(filepath.Ext(cmd)) {
		case ".py", ".js", ".mjs", ".sh":
			problems = append(problems, fmt.Sprintf("%s %q needs a host interpreter; bundle a target-native executable", label, cmd))
			return
		}
		if filepath.IsAbs(cmd) || (!strings.Contains(cmd, "/") && !strings.Contains(cmd, `\`)) {
			problems = append(problems, fmt.Sprintf("%s uses host command %q; use a unit-relative bundled executable", label, cmd))
			return
		}
		clean := filepath.Clean(filepath.FromSlash(cmd))
		if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			problems = append(problems, fmt.Sprintf("%s command escapes the unit", label))
			return
		}
		if info, e := os.Stat(filepath.Join(tmp, clean)); e != nil || info.IsDir() {
			problems = append(problems, fmt.Sprintf("%s executable %q is not bundled", label, cmd))
		}
	}
	if m.Entrypoint.Type == "script" {
		checkCommand("entrypoint", []string{m.Entrypoint.Script})
	}
	for _, tool := range m.Tools {
		checkCommand("tool "+tool.Name, tool.Exec.Command)
	}
	if len(problems) > 0 {
		return fmt.Errorf("offline package is incomplete for %s/%s%s:\n  - %s", chosen.OS, chosen.Arch, map[bool]string{true: "/" + chosen.Device, false: ""}[chosen.Device != ""], strings.Join(problems, "\n  - "))
	}
	return nil
}
