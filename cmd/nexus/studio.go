package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/studio"
)

func newStudioCmd() *cobra.Command {
	var addr, workspace string
	var modelDirs []string
	var noBrowser, app, quitWhenClosed, installLauncher, removeLauncher bool
	cmd := &cobra.Command{
		Use:   "studio [dir]",
		Short: "Build a workflow on a canvas",
		Long: `Open the workflow editor on a project folder (default: the current one). Drag
agents onto the canvas, drag a model from your machine onto an agent, and drag
from one agent to another to send its output there. Press Run to watch it work.
Studio edits the same nexus-compose.yaml that 'nexus compose validate' and
'nexus compose up' use, and a unit per agent under agents/.

The editor is a local web page. It listens on a loopback address only, needs an
access token, and can only read and write a few files inside the project folder.

With --app it opens in its own window, without browser toolbars, using Chrome,
Edge, Chromium or Brave when one is installed.

With --workspace the editor opens on a folder of workflows instead of one
workflow: it lists them, and creates new ones inside it. --install-launcher adds
NexusRun Studio to your application menu (Linux), opening that workspace in its
own window and stopping when the window is closed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if installLauncher || removeLauncher {
				return launcherCommand(workspace, removeLauncher)
			}
			dir := "."
			if len(args) == 1 {
				if workspace != "" {
					return fmt.Errorf("give a folder or --workspace, not both")
				}
				dir = args[0]
			}
			if workspace != "" {
				dir = ""
			} else if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return fmt.Errorf("%s is not a folder", dir)
			}
			// The usual port is a convenience, so a second editor takes any free
			// one. An address the user chose is respected and fails if taken.
			if !cmd.Flags().Changed("addr") {
				if ln, err := net.Listen("tcp", addr); err != nil {
					addr = "127.0.0.1:0"
				} else {
					ln.Close()
				}
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
			return studio.Serve(ctx, studio.Options{Dir: dir, Workspace: workspace, Addr: addr, Store: s, ModelDirs: modelDirs, QuitWhenClosed: quitWhenClosed}, func(l studio.Links) {
				fmt.Printf("NexusRun Studio → %s\nPress Ctrl+C to stop.\n", l.Token)
				if noBrowser {
					return
				}
				// What is handed to another program ends up on a command line, which
				// other accounts on this machine can read, so it gets the one-time
				// code and not the token.
				if app {
					if exe := findAppBrowser(); exe != "" {
						if openAppWindow(exe, l.Code) == nil {
							return
						}
					}
					fmt.Println("No Chrome, Edge, Chromium or Brave found for an app window; using your default browser.")
				}
				if canOpenBrowser() {
					_ = openBrowser(l.Code)
				}
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7718", "listen address (loopback only)")
	cmd.Flags().StringArrayVar(&modelDirs, "model-dir", nil, "also search this folder for .gguf models (repeatable)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening anything")
	cmd.Flags().BoolVar(&app, "app", false, "open in its own window instead of a browser tab")
	cmd.Flags().StringVar(&workspace, "workspace", "", "open a folder of workflows instead of a single workflow")
	cmd.Flags().BoolVar(&quitWhenClosed, "quit-when-closed", false, "stop when the page is closed (for launchers that have no terminal)")
	cmd.Flags().BoolVar(&installLauncher, "install-launcher", false, "add NexusRun Studio to the application menu (Linux) and exit")
	cmd.Flags().BoolVar(&removeLauncher, "remove-launcher", false, "remove the application menu entry and exit")
	return cmd
}

// appBrowsers are the Chromium-based browsers that can show a page as an app
// window (--app), by platform. Names are looked up on PATH; paths are used as is.
func appBrowserCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		}
	case "windows":
		var out []string
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			if root == "" {
				continue
			}
			out = append(out,
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(root, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"))
		}
		return out
	default:
		return []string{"google-chrome", "google-chrome-stable", "microsoft-edge", "brave-browser", "chromium", "chromium-browser"}
	}
}

// findAppBrowser returns the first installed browser that can open an app window.
func findAppBrowser() string {
	for _, c := range appBrowserCandidates() {
		if filepath.IsAbs(c) {
			if info, err := os.Stat(c); err == nil && !info.IsDir() {
				return c
			}
		} else if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// openAppWindow starts the browser in app mode on url and does not wait for it.
func openAppWindow(exe, url string) error {
	cmd := exec.Command(exe, "--app="+url, "--no-first-run", "--no-default-browser-check")
	return cmd.Start()
}
