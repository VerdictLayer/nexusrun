//go:build !linux

// Sandboxing is currently Linux-only. On other platforms enforcement is
// unavailable, and callers must decide whether to proceed — NexusRun
// refuses to run script units unsandboxed unless explicitly told to.
package sandbox

import "errors"

// ErrUnsupported reports that this platform cannot enforce a policy.
var ErrUnsupported = errors.New("sandboxing is only implemented on Linux (Landlock)")

// Supported reports whether this platform can enforce a policy.
func Supported() bool { return false }

// Describe returns a human-readable summary of enforcement support.
func Describe() string {
	return "unavailable (Landlock is Linux-only; macOS Seatbelt and Windows AppContainer are not implemented)"
}

// Apply always fails on unsupported platforms, so a caller can never
// mistake "no sandbox" for "sandboxed".
func Apply(p Policy) error { return ErrUnsupported }
