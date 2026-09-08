package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	installer "github.com/verdictlayer/nexusrun/internal/install"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func newInstallCmd() *cobra.Command {
	var yes, jsonOut, noTests bool
	var secretArgs []string
	cmd := &cobra.Command{
		Use:   "install <ref|dir>",
		Short: "Prepare a unit and prove it works on this machine",
		Long: `Install checks the unit as a whole: model weights, an inference runtime,
hardware and memory, MCP tools, credentials, and the acceptance suite shipped
with the unit. Downloads and runtime installation require explicit approval.

Use --secret KEY=value to configure a declared credential in the existing
encrypted secret store. Prefer --secret-file workflows through "nexus secret
set" when shell history is a concern.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			plan, err := installer.Inspect(cmd.Context(), s, args[0], installer.Options{InspectDownloadSizes: true})
			if err != nil {
				return err
			}
			if jsonOut && !yes {
				return writeInstallJSON(cmd.OutOrStdout(), plan)
			}
			if !jsonOut {
				printInstallPlan(cmd.OutOrStdout(), plan)
			}
			req := installer.Request{Secrets: map[string]string{}, RunTests: !noTests}
			for _, v := range secretArgs {
				k, val, ok := strings.Cut(v, "=")
				if !ok || k == "" {
					return fmt.Errorf("--secret must be KEY=value, got %q", v)
				}
				req.Secrets[k] = val
			}
			for _, c := range plan.Checks {
				switch c.Action {
				case "download_model":
					req.Models = true
				case "install_runtime":
					req.Runtime = true
				case "install_tools":
					req.Tools = true
				}
			}
			needsApproval := req.Models || req.Runtime || req.Tools
			if needsApproval && !yes {
				ok, e := confirmInstall(cmd.InOrStdin(), cmd.OutOrStdout())
				if e != nil {
					return e
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "Installation paused; nothing was downloaded.")
					return nil
				}
			}
			req.Progress = func(kind, message string, done, total int64) {
				if jsonOut {
					return
				}
				if done > 0 && total > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s — %.0f%%\n", kind, message, 100*float64(done)/float64(total))
				} else if done == 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s\n", kind, message)
				}
			}
			result, err := installer.Apply(cmd.Context(), s, args[0], req)
			if err != nil {
				return err
			}
			if jsonOut {
				if err := writeInstallJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
				if !result.Complete {
					return fmt.Errorf("installation is incomplete; inspect the JSON checks for required actions or failed tests")
				}
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout())
			printInstallPlan(cmd.OutOrStdout(), result)
			if !result.Complete {
				return fmt.Errorf("installation needs the actions shown above; complete them and rerun %s install %s", binName, args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nInstalled %s; all required checks completed.\n", result.Unit)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve all model, runtime, and tool downloads")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the installation plan/result as JSON (does not prompt)")
	cmd.Flags().BoolVar(&noTests, "no-tests", false, "skip included acceptance tests")
	cmd.Flags().StringArrayVar(&secretArgs, "secret", nil, "store a declared secret as KEY=value (repeatable)")
	return cmd
}

func confirmInstall(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "\nApprove these downloads and installations? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

func writeInstallJSON(w io.Writer, p *installer.Plan) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

func printInstallPlan(w io.Writer, p *installer.Plan) {
	fmt.Fprintf(w, "Installation plan for %s\n", p.Unit)
	for _, c := range p.Checks {
		mark := map[string]string{installer.Ready: "✓", installer.Passed: "✓", installer.ActionRequired: "!", installer.Blocked: "✗", installer.Skipped: "–"}[c.Status]
		if mark == "" {
			mark = "·"
		}
		fmt.Fprintf(w, "  %s %-8s %s", mark, c.Kind, c.Summary)
		if c.SizeBytes > 0 {
			fmt.Fprintf(w, " (%s)", humanSize(c.SizeBytes))
		}
		fmt.Fprintln(w)
		if c.Detail != "" && (c.Status != installer.Ready || c.Kind == "hardware") {
			fmt.Fprintf(w, "             %s\n", c.Detail)
		}
		if c.Command != "" {
			fmt.Fprintf(w, "             Next: %s\n", c.Command)
		}
	}
	if c := p.Configuration; c != nil {
		fmt.Fprintf(w, "\nTested configuration: %s/%s, context %d (%s available; ~%s required)\n", c.Backend, c.Device, c.Context, humanSize(c.MemoryAvailableMB*1024*1024), humanSize(c.MemoryRequiredMB*1024*1024))
	}
	if t := p.Tests; t != nil {
		fmt.Fprintf(w, "Acceptance tests: %s", t.Status)
		if t.Suite != "" {
			fmt.Fprintf(w, " — %s", t.Suite)
		}
		if t.Passed+t.Failed > 0 {
			fmt.Fprintf(w, " (%d passed, %d failed)", t.Passed, t.Failed)
		}
		fmt.Fprintln(w)
		for _, c := range t.Cases {
			if c.Status != installer.Pending {
				fmt.Fprintf(w, "  %-6s %s", c.Status, c.Name)
				if c.Detail != "" {
					fmt.Fprintf(w, ": %s", c.Detail)
				}
				fmt.Fprintln(w)
			}
		}
	}
}
