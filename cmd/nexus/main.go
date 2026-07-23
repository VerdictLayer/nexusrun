// Command nexus is the NexusRun CLI: build, run, and share portable AI units.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is stamped at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
)

// binName is the single place the CLI's name appears, so rebranding the
// project is a one-line change.
const binName = "nexus"

func main() {
	root := &cobra.Command{
		Use:   binName,
		Short: "Portable AI units — build once, run on any hardware",
		Long: `NexusRun packages an AI agent — its models, tools, prompts, and config —
into a portable OCI artifact that runs on laptops, servers, and edge devices
without containers, and picks the fastest accelerator available on each host.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (%s)", Version, Commit),
	}

	root.AddCommand(
		newInitCmd(),
		newBuildCmd(),
		newRunCmd(),
		newComposeCmd(),
		newListCmd(),
		newInspectCmd(),
		newPushCmd(),
		newPullCmd(),
		newExportCmd(),
		newImportCmd(),
		newBenchCmd(),
		newDoctorCmd(),
		newModelsCmd(),
		newLogsCmd(),
		newServeCmd(),
		newSandboxExecCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", binName, err)
		os.Exit(1)
	}
}
