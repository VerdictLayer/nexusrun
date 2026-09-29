package bundle

import (
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/runtimepkg"
)

func TestLauncherIsRelocatableAndLocked(t *testing.T) {
	s := launcherScript("demo:1.0.0", runtimeMeta())
	for _, want := range []string{"dirname -- \"$0\"", "runtime verify-dir", "NEXUSRUN_LLAMA_SERVER", "NEXUSRUN_LLAMA_CLI", "--no-daemon --backend llama.cpp/server", "agent/demo.nx"} {
		if !contains(s, want) {
			t.Errorf("launcher missing %q", want)
		}
	}
}
func runtimeMeta() *runtimepkg.Metadata {
	return &runtimepkg.Metadata{Executable: "bin/llama-server", CLIExecutable: "bin/llama-cli", LibraryPaths: []string{"lib"}}
}
func contains(s, p string) bool { return strings.Contains(s, p) }
