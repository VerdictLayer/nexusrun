package runner

import "errors"

// sandboxHelperCommand is the hidden subcommand that applies a sandbox
// policy to itself and then execs the real program.
const sandboxHelperCommand = "__sandbox-exec"

// HelperCommandName exposes the hidden subcommand name to the CLI.
func HelperCommandName() string { return sandboxHelperCommand }

// HelperOptions are the policy inputs passed to the helper process.
type HelperOptions struct {
	WorkDir   string
	HomeDir   string
	Network   bool
	Storage   bool
	ReadPaths []string
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
