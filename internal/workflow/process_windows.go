//go:build windows

package workflow

import (
	"os"
	"os/exec"
	"syscall"
)

// Detach hides the child's console window so `compose up -d` does not
// flash a terminal. Windows has no session leader to become; a process
// started without an inherited console already outlives its parent.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008} // DETACHED_PROCESS
}

// Alive reports whether a pid is a live process.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// FindProcess succeeds for any pid on Windows, so ask the OS whether
	// the handle still refers to something running.
	if _, err := p.Wait(); err == nil {
		return false
	}
	return true
}

// Stop terminates a workflow. Windows has no SIGTERM to deliver to a
// detached process, so this is an outright kill; the state bus is flushed
// per message, which is why that costs history rather than everything.
func Stop(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
