//go:build windows

package runner

import "errors"

// RunHelper is unavailable on Windows: there is no Landlock equivalent
// wired up, and exec-replacement semantics differ. Script units must be
// run with --no-sandbox there, which the caller reports explicitly.
func RunHelper(opts HelperOptions, argv []string) error {
	return errors.New("sandboxed execution is not implemented on Windows")
}
