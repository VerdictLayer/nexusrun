package sandbox

// Policy describes what a unit is allowed to touch. It is derived from
// the unit's declared `capabilities:` — anything not declared is denied.
type Policy struct {
	// AllowNetwork permits network sockets. When false, a seccomp filter
	// refuses every socket except local (AF_UNIX) and netlink ones, so a
	// unit cannot open TCP, UDP (including DNS) or raw connections on any
	// kernel. Abstract AF_UNIX sockets are not scoped.
	AllowNetwork bool

	// ReadOnlyPaths are readable but not writable, in addition to the
	// system directories needed to start an interpreter.
	ReadOnlyPaths []string

	// ReadWritePaths are fully accessible — typically just the unit's
	// own working directory.
	ReadWritePaths []string
}

// FromCapabilities builds a policy from a unit's declared capabilities.
//
// homeDir must be supplied by the caller rather than read from $HOME:
// script units run with HOME pointing at their own working directory, so
// the environment cannot be trusted to name the real user home.
func FromCapabilities(caps []string, workDir, homeDir string, readOnly ...string) Policy {
	p := Policy{
		ReadWritePaths: []string{workDir},
		ReadOnlyPaths:  readOnly,
	}
	for _, c := range caps {
		switch c {
		case "network":
			p.AllowNetwork = true
		case "storage":
			// Storage beyond the unit's own directory means the user's
			// home; still nothing outside it.
			if homeDir != "" {
				p.ReadWritePaths = append(p.ReadWritePaths, homeDir)
			}
		}
	}
	return p
}
