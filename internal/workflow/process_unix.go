//go:build !windows

package workflow

import (
	"os"
	"os/exec"
	"syscall"
)

// Detach puts a child in its own session so it survives the terminal that
// started it. Without this, `compose up -d` would die with the shell.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Alive reports whether a pid is a live process. Signal 0 performs the
// permission and existence checks without delivering anything.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// Stop asks a workflow to shut down. SIGTERM rather than SIGKILL: the run
// is mid-generation and holding a state file open, and a clean exit
// flushes what it has already produced.
func Stop(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}
