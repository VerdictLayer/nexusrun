package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/verdictlayer/nexusrun/internal/bundle"
	"github.com/verdictlayer/nexusrun/internal/runtimepkg"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func newRuntimeCmd() *cobra.Command {
	c := &cobra.Command{Use: "runtime", Short: "Manage pinned local inference runtimes"}
	c.AddCommand(newRuntimeImportCmd(), newRuntimeListCmd(), newRuntimeInspectCmd(), newRuntimeVerifyCmd(), newRuntimeVerifyDirCmd(), newRuntimeActivateCmd())
	return c
}
func newRuntimeImportCmd() *cobra.Command {
	var activate bool
	c := &cobra.Command{Use: "import <runtime.tar.gz>", Short: "Import and verify a local runtime package", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := store.Open()
		if e != nil {
			return e
		}
		r, e := runtimepkg.Import(s.Root, args[0])
		if e != nil {
			return e
		}
		if activate {
			if e = runtimepkg.Activate(s.Root, r.ID); e != nil {
				return e
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Imported %s\n", r.ID)
		return nil
	}}
	c.Flags().BoolVar(&activate, "activate", false, "select this compatible runtime for ordinary runs")
	return c
}
func newRuntimeListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List managed runtimes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		s, e := store.Open()
		if e != nil {
			return e
		}
		rs, e := runtimepkg.List(s.Root)
		if e != nil {
			return e
		}
		for _, r := range rs {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", r.ID, r.Path)
		}
		return nil
	}}
}
func runtimeShow(verify bool) *cobra.Command {
	verb := "inspect"
	if verify {
		verb = "verify"
	}
	return &cobra.Command{Use: verb + " <runtime-id>", Short: map[bool]string{true: "Verify every runtime file", false: "Show runtime metadata"}[verify], Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := store.Open()
		if e != nil {
			return e
		}
		if verify {
			r, e := runtimepkg.Verify(s.Root, args[0])
			if e != nil {
				return e
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Verified %s\n", r.ID)
			return nil
		}
		_, m, e := runtimepkg.Resolve(s.Root, args[0])
		if e != nil {
			return e
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	}}
}
func newRuntimeInspectCmd() *cobra.Command { return runtimeShow(false) }
func newRuntimeVerifyCmd() *cobra.Command  { return runtimeShow(true) }
func newRuntimeVerifyDirCmd() *cobra.Command {
	return &cobra.Command{Use: "verify-dir <directory>", Short: "Verify an unpacked runtime directory", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		m, err := runtimepkg.VerifyDirectory(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Verified %s\n", m.ID())
		return nil
	}}
}
func newRuntimeActivateCmd() *cobra.Command {
	return &cobra.Command{Use: "activate <runtime-id>", Short: "Select a managed runtime for ordinary runs", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := store.Open()
		if e != nil {
			return e
		}
		if e = runtimepkg.Activate(s.Root, args[0]); e != nil {
			return e
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Activated %s\n", args[0])
		return nil
	}}
}

func newBundleCmd() *cobra.Command {
	var target, runtimeID, nexusPath, out string
	c := &cobra.Command{Use: "bundle <ref>", Short: "Export a complete target-specific offline deployment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := store.Open()
		if e != nil {
			return e
		}
		if nexusPath == "" {
			if target == runtime.GOOS+"/"+runtime.GOARCH {
				nexusPath, e = os.Executable()
				if e != nil {
					return e
				}
			} else {
				return fmt.Errorf("--nexus is required when bundling for a different target")
			}
		}
		m, e := bundle.Export(cmd.Context(), s, bundle.Options{Ref: args[0], Target: target, RuntimeID: runtimeID, NexusPath: nexusPath, Output: out})
		if e != nil {
			return e
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Bundled %s for %s → %s\n  unit: %s\n  runtime: %s\n", m.Unit, m.Target, out, m.UnitDigest, m.RuntimeDigest)
		return nil
	}}
	c.Flags().StringVar(&target, "target", "", "target os/arch")
	c.Flags().StringVar(&runtimeID, "runtime", "", "exact managed runtime id")
	c.Flags().StringVar(&nexusPath, "nexus", "", "target NexusRun executable (defaults to this executable for the host target)")
	c.Flags().StringVarP(&out, "output", "o", "", "output .tar.gz")
	_ = c.MarkFlagRequired("target")
	_ = c.MarkFlagRequired("runtime")
	_ = c.MarkFlagRequired("output")
	return c
}
