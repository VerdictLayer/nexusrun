// Package credentials stores registry logins made with `nexus login`.
//
// The file lives in the NexusRun home directory and is written with mode 0600
// inside a 0700 directory. A file readable by other users is refused rather
// than trusted, because it holds a bearer secret. Environment variables
// (NEXUSRUN_REGISTRY_USER / _PASSWORD) always take precedence, which keeps CI
// and containers working unchanged.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Entry is one registry login.
type Entry struct {
	Token   string `json:"token"`
	Org     string `json:"org,omitempty"`
	Scope   string `json:"scope,omitempty"` // "push" or "pull"
	Account string `json:"account,omitempty"`
}

type fileFormat struct {
	Registries map[string]Entry `json:"registries"`
}

// ErrInsecure means the credentials file is accessible to other users.
var ErrInsecure = errors.New("credentials file is accessible by other users")

// Path returns the credentials file location: $NEXUSRUN_HOME or ~/.nexusrun.
func Path() (string, error) {
	root := os.Getenv("NEXUSRUN_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".nexusrun")
	}
	return filepath.Join(root, "credentials.json"), nil
}

func normalize(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "/"))
}

func load(path string) (fileFormat, error) {
	f := fileFormat{Registries: map[string]Entry{}}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return f, fmt.Errorf("%w: %s has mode %o; run chmod 600", ErrInsecure, path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s is not valid: %w", path, err)
	}
	if f.Registries == nil {
		f.Registries = map[string]Entry{}
	}
	return f, nil
}

// Lookup returns the stored login for a registry host.
func Lookup(host string) (Entry, bool, error) {
	path, err := Path()
	if err != nil {
		return Entry{}, false, err
	}
	f, err := load(path)
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := f.Registries[normalize(host)]
	return e, ok && e.Token != "", nil
}

// Save stores a login, replacing any earlier one for the same host.
func Save(host string, e Entry) error {
	path, err := Path()
	if err != nil {
		return err
	}
	f, err := load(path)
	if err != nil && !errors.Is(err, ErrInsecure) {
		return err
	}
	if errors.Is(err, ErrInsecure) {
		// We are about to rewrite the file with safe permissions anyway.
		f = fileFormat{Registries: map[string]Entry{}}
		if b, rerr := os.ReadFile(path); rerr == nil {
			_ = json.Unmarshal(b, &f)
			if f.Registries == nil {
				f.Registries = map[string]Entry{}
			}
		}
	}
	f.Registries[normalize(host)] = e
	return write(path, f)
}

// Delete removes a login, reporting whether there was one.
func Delete(host string) (bool, error) {
	path, err := Path()
	if err != nil {
		return false, err
	}
	f, err := load(path)
	if err != nil {
		return false, err
	}
	h := normalize(host)
	if _, ok := f.Registries[h]; !ok {
		return false, nil
	}
	delete(f.Registries, h)
	return true, write(path, f)
}

// write replaces the file atomically so a crash never leaves a half-written
// (or briefly world-readable) credentials file.
func write(path string, f fileFormat) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
