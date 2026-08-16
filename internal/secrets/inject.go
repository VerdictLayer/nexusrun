package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/manifest"
)

// Injection is a resolved set of values ready to hand to a unit, plus the
// cleanup for anything it had to write to disk.
type Injection struct {
	// Env is the variables to add, as KEY=VALUE.
	Env []string

	// Files are secrets written to disk because the unit asked for them as
	// files (mount_path) rather than variables — certificates and key
	// files, mostly.
	Files []string

	// Missing names optional secrets that were not found. Required ones
	// are an error instead, so this list is only ever advisory.
	Missing []string

	cleanup []string

	// ownedDir is the temporary directory Inject created for mounted
	// secrets, and the only directory Close may remove. A caller-supplied
	// MountRoot is theirs, and deleting it once it happened to be empty
	// would be a surprise well outside what an injection is for.
	ownedDir string
}

// Close removes any files written for this injection. It is safe to call
// more than once.
func (in *Injection) Close() {
	for _, p := range in.cleanup {
		_ = os.Remove(p)
	}
	if in.ownedDir != "" {
		_ = os.Remove(in.ownedDir)
		in.ownedDir = ""
	}
	in.cleanup = nil
}

// InjectOptions configures resolution.
type InjectOptions struct {
	// Device selects device-scoped secrets.
	Device string

	// Grace is how long a rotated secret's previous value is also offered,
	// as KEY_PREVIOUS.
	Grace time.Duration

	// MountRoot is where mount_path secrets are actually written. A unit
	// declaring an absolute path like /etc/nexus/certs/ssl.pem must not
	// cause the runtime to write to /etc, so the path is reproduced *under*
	// this root and the unit is told where it really landed.
	MountRoot string
}

// Inject resolves a unit's declared secrets and config.
//
// Required secrets that are missing are an error: an agent started without
// its credentials does not fail cleanly, it fails somewhere in the middle
// of a request against a third party, which is far harder to read.
func (st *Store) Inject(m *manifest.Manifest, opts InjectOptions) (*Injection, error) {
	in := &Injection{}

	// Config first, so a secret can never be shadowed by a config default
	// — validation already rejects a collision, and this makes the order
	// explicit rather than incidental.
	for _, c := range m.Config {
		in.Env = append(in.Env, c.EnvName()+"="+c.Default)
	}

	if len(m.Secrets) == 0 {
		return in, nil
	}

	keys := make([]string, 0, len(m.Secrets))
	for _, s := range m.Secrets {
		keys = append(keys, s.Name)
	}
	resolved, missing, err := st.Env(m.Name, opts.Device, keys, opts.Grace)
	if err != nil {
		return nil, err
	}
	missingSet := map[string]bool{}
	for _, k := range missing {
		missingSet[k] = true
	}

	var required []string
	for _, s := range m.Secrets {
		if !missingSet[s.Name] {
			continue
		}
		if s.Required {
			required = append(required, s.Name)
		}
		in.Missing = append(in.Missing, s.Name)
	}
	if len(required) > 0 {
		sort.Strings(required)
		return nil, fmt.Errorf(
			"%s requires %d secret(s) that are not stored: %s\n  set them with: %s",
			m.Ref(), len(required), strings.Join(required, ", "),
			suggest(m.Name, required))
	}

	for _, s := range m.Secrets {
		r, ok := resolved[s.Name]
		if !ok {
			continue
		}
		if s.MountPath == "" {
			in.Env = append(in.Env, s.EnvName()+"="+r.Value)
			if r.Previous != "" {
				in.Env = append(in.Env, s.EnvName()+"_PREVIOUS="+r.Previous)
			}
			continue
		}
		root := opts.MountRoot
		if root == "" {
			if in.ownedDir == "" {
				dir, derr := os.MkdirTemp("", "nexus-secrets-*")
				if derr != nil {
					in.Close()
					return nil, derr
				}
				in.ownedDir = dir
			}
			root = in.ownedDir
		}
		path, werr := writeMounted(root, s.MountPath, r.Value)
		if werr != nil {
			in.Close()
			return nil, werr
		}
		in.cleanup = append(in.cleanup, path)
		in.Files = append(in.Files, path)
		// The unit is told where the file actually is. Honouring the
		// declared absolute path literally would mean writing to /etc.
		in.Env = append(in.Env, s.EnvName()+"="+path)
	}
	return in, nil
}

// writeMounted places a secret file under root, mirroring the declared
// path so a unit expecting ".../ssl.pem" still sees that basename.
func writeMounted(root, declared, value string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// Only the base name is used. A declared path is a hint about the
	// filename the unit expects, never permission to write where it says.
	path := filepath.Join(root, filepath.Base(filepath.FromSlash(declared)))
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func suggest(agent string, keys []string) string {
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("nexus secret set %s %s --stdin", agent, k))
	}
	return strings.Join(parts, "\n                     ")
}
