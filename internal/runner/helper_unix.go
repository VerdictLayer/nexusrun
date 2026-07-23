//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/verdictlayer/nexusrun/internal/sandbox"
)

// RunHelper applies the sandbox policy to the current process and then
// replaces it with the target program. It never returns on success.
func RunHelper(opts HelperOptions, argv []string) error {
	if len(argv) == 0 {
		return errors.New("no command given to sandbox helper")
	}

	caps := []string{}
	if opts.Network {
		caps = append(caps, "network")
	}
	if opts.Storage {
		caps = append(caps, "storage")
	}
	policy := sandbox.FromCapabilities(caps, opts.WorkDir, opts.HomeDir, opts.ReadPaths...)

	if err := sandbox.Apply(policy); err != nil {
		return fmt.Errorf("could not apply sandbox: %w", err)
	}

	bin := argv[0]
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	}
	// Exec rather than fork: the sandboxed process replaces this one, so
	// no unsandboxed parent is left holding privileges.
	return syscall.Exec(bin, argv, os.Environ())
}
