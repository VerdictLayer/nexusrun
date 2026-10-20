package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/studio"
)

func newStudioCmd() *cobra.Command {
	var addr string
	var modelDirs []string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "studio [dir]",
		Short: "Build a workflow on a canvas",
		Long: `Open the workflow editor on a project folder (default: the current one). Drag
agents onto the canvas, drag a model from your machine onto an agent, and drag
from one agent to another to send its output there. Studio edits the same
nexus-compose.yaml that 'nexus compose validate' and 'nexus compose up' use, and
a unit per agent under agents/.

The editor is a local web page. It listens on a loopback address only, needs the
access token in the link this command prints, and can only read and write a few
files inside the project folder.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return fmt.Errorf("%s is not a folder", dir)
			}
			s, err := store.Open()
			if err != nil {
				return err
			}
			if env := os.Getenv("NEXUSRUN_MODEL_DIRS"); env != "" {
				modelDirs = append(modelDirs, filepath.SplitList(env)...)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return studio.Serve(ctx, studio.Options{Dir: dir, Addr: addr, Store: s, ModelDirs: modelDirs}, func(link string) {
				fmt.Printf("NexusRun Studio → %s\nPress Ctrl+C to stop.\n", link)
				if !noBrowser && canOpenBrowser() {
					_ = openBrowser(link)
				}
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7718", "listen address (loopback only)")
	cmd.Flags().StringArrayVar(&modelDirs, "model-dir", nil, "also search this folder for .gguf models (repeatable)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening a browser")
	return cmd
}
