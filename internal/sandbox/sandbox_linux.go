//go:build linux

// Package sandbox confines script units using Landlock, the unprivileged
// Linux LSM. A unit declares what it needs in `capabilities:`; anything
// it did not declare is denied by the kernel, not by convention.
//
// Landlock restrictions are inherited across exec and cannot be dropped,
// which is exactly the property needed here: the policy is applied in a
// helper process that then execs the unit's interpreter.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/landlock-lsm/go-landlock/landlock"
	lls "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// Supported reports whether this kernel can enforce a policy.
//
// This must query the ABI version rather than applying a ruleset:
// calling RestrictPaths with no rules does not test support, it enforces
// a deny-everything policy on the calling process — which would sandbox
// the CLI itself before it could exec anything.
func Supported() bool {
	v, err := lls.LandlockGetABIVersion()
	return err == nil && v >= 1
}

// Describe returns a human-readable summary of enforcement support.
func Describe() string {
	v, err := lls.LandlockGetABIVersion()
	if err != nil || v < 1 {
		return "unavailable (kernel lacks Landlock)"
	}
	if v >= 4 {
		return fmt.Sprintf("Landlock ABI v%d (filesystem + network)", v)
	}
	return fmt.Sprintf("Landlock ABI v%d (filesystem only; network rules need ABI 4+)", v)
}

// existing filters a path list down to what is actually present.
// Landlock rejects rules naming a nonexistent path, so a missing
// device on a minimal container image must not fail the whole policy.
func existing(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// classify splits paths into directories and regular files.
//
// Landlock rules are typed: a directory rule carries directory access
// rights, and applying those to a regular file is rejected outright with
// "inconsistent access rights". Anything that cannot be stat'd is dropped
// for the same reason `existing` exists — a rule naming a nonexistent path
// fails the whole ruleset.
func classify(paths []string) (dirs, files []string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if info.IsDir() {
			dirs = append(dirs, abs)
		} else if info.Mode().IsRegular() {
			files = append(files, abs)
		}
	}
	return dirs, files
}

// Apply enforces the policy on the current process. It is irreversible,
// and every child process inherits it.
func Apply(p Policy) error {
	roDirs := []string{}

	// Interpreters and shared libraries must stay readable or nothing
	// runs at all.
	for _, d := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin", "/etc", "/opt"} {
		if _, err := os.Stat(d); err == nil {
			roDirs = append(roDirs, d)
		}
	}

	// A read-only grant is routinely a single file rather than a tree — a
	// model's weights, or a mounted secret — so each path is classified
	// instead of being assumed to be a directory.
	extraRO, roFiles := classify(p.ReadOnlyPaths)
	roDirs = append(roDirs, extraRO...)
	rwDirs, rwFiles := classify(p.ReadWritePaths)

	rules := []landlock.Rule{}
	if len(roDirs) > 0 {
		rules = append(rules, landlock.RODirs(roDirs...))
	}
	if len(roFiles) > 0 {
		rules = append(rules, landlock.ROFiles(roFiles...))
	}
	if len(rwDirs) > 0 {
		rules = append(rules, landlock.RWDirs(rwDirs...))
	}
	if len(rwFiles) > 0 {
		rules = append(rules, landlock.RWFiles(rwFiles...))
	}

	// The standard character devices, granted individually rather than by
	// opening up /dev. Without these almost nothing runs: a bare
	// `cmd 2>/dev/null` fails, and language runtimes seed their hash and
	// RNG state from /dev/urandom before executing a single line of user
	// code. These leak nothing — /dev/null discards, /dev/zero and the
	// random devices are stateless sources.
	if rw := existing("/dev/null", "/dev/zero", "/dev/full", "/dev/tty"); len(rw) > 0 {
		rules = append(rules, landlock.RWFiles(rw...))
	}
	if ro := existing("/dev/random", "/dev/urandom"); len(ro) > 0 {
		rules = append(rules, landlock.ROFiles(ro...))
	}

	// BestEffort keeps this working on kernels with older Landlock ABIs
	// rather than failing closed on the whole feature.
	cfg := landlock.V5.BestEffort()
	if err := cfg.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("apply filesystem policy: %w", err)
	}

	if !p.AllowNetwork {
		// With no ConnectTCP rules, all outbound TCP is denied.
		// Landlock governs TCP only; see Policy.AllowNetwork.
		if err := landlock.V5.BestEffort().RestrictNet(); err != nil {
			return fmt.Errorf("apply network policy: %w", err)
		}
	}
	return nil
}
