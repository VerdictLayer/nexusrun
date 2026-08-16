package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/mcp"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

func newToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Manage the MCP servers a unit depends on",
		Long: `A unit can declare MCP servers the way it declares models: by source,
version-pinned, fetched into a shared local cache, and started under the
same kernel sandbox as everything else.

  nexus tools install my-agent   # fetch what it declares
  nexus tools check my-agent     # start each one and list its tools
  nexus tools ls                 # what is cached on this machine`,
	}
	cmd.AddCommand(
		newToolsInstallCmd(),
		newToolsListCmd(),
		newToolsCheckCmd(),
		newToolsUpdateCmd(),
	)
	return cmd
}

// loadUnitFor resolves a ref or directory to a manifest and its source
// directory, unpacking a built unit into a temporary tree.
func loadUnitFor(ctx context.Context, s *store.Store, ref string) (*manifest.Manifest, string, func(), error) {
	noop := func() {}
	if info, err := os.Stat(ref); err == nil && info.IsDir() {
		m, lerr := manifest.Load(ref)
		if lerr != nil {
			return nil, "", noop, lerr
		}
		return m, ref, noop, nil
	}
	m, _, err := unit.Resolve(ctx, s, ref)
	if err != nil {
		return nil, "", noop, err
	}
	tmp, err := os.MkdirTemp("", "nexus-tools-*")
	if err != nil {
		return nil, "", noop, err
	}
	if _, err := unit.Unpack(ctx, s, ref, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, "", noop, err
	}
	return m, tmp, func() { os.RemoveAll(tmp) }, nil
}

func newToolsInstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install <ref|dir>",
		Short: "Fetch the MCP servers a unit declares",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			m, _, cleanup, err := loadUnitFor(ctx, s, args[0])
			if err != nil {
				return err
			}
			defer cleanup()

			if len(m.MCPServers) == 0 {
				fmt.Printf("%s declares no MCP servers.\n", m.Ref())
				return nil
			}
			for _, name := range m.MCPNames() {
				srv := m.MCPServers[name]
				src, perr := mcp.ParseSource(srv.Source)
				if perr != nil {
					return fmt.Errorf("mcp_servers.%s: %w", name, perr)
				}
				if !src.Pinned() {
					logf("warning: %s tracks a floating ref (%s) — pin it to a commit to make this unit reproducible",
						name, srv.Source)
				}
				logf("installing %s from %s", name, srv.Source)
				if err := src.Install(ctx, s, force, logf); err != nil {
					return fmt.Errorf("mcp_servers.%s: %w", name, err)
				}
				fmt.Printf("  %-16s %s\n", name, src.InstallDir(s))
			}
			fmt.Printf("\nCheck them with: %s tools check %s\n", binName, args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "re-fetch even if already cached")
	return cmd
}

func newToolsUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <ref|dir>",
		Short: "Re-fetch a unit's MCP servers",
		Long: `Update discards the cached copy and fetches again.

For a pinned source this is a no-op by construction — that is the point of
pinning. It matters for a unit tracking a branch, which is also the unit
whose behaviour can change without its digest changing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			m, _, cleanup, err := loadUnitFor(ctx, s, args[0])
			if err != nil {
				return err
			}
			defer cleanup()

			for _, name := range m.MCPNames() {
				src, perr := mcp.ParseSource(m.MCPServers[name].Source)
				if perr != nil {
					return fmt.Errorf("mcp_servers.%s: %w", name, perr)
				}
				logf("updating %s", name)
				if err := src.Install(ctx, s, true, logf); err != nil {
					return fmt.Errorf("mcp_servers.%s: %w", name, err)
				}
			}
			fmt.Printf("Updated %d server(s).\n", len(m.MCPServers))
			return nil
		},
	}
	return cmd
}

func newToolsListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "list [ref|dir]",
		Aliases: []string{"ls"},
		Short:   "Show cached MCP servers, or what a unit declares",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}

			if len(args) == 0 {
				dir := mcp.CacheDir(s)
				entries, rerr := collectCached(dir)
				if rerr != nil {
					return rerr
				}
				if jsonOut {
					enc := json.NewEncoder(os.Stdout)
					enc.SetIndent("", "  ")
					return enc.Encode(entries)
				}
				if len(entries) == 0 {
					fmt.Printf("No MCP servers cached. Install a unit's with:\n  %s tools install <unit>\n", binName)
					return nil
				}
				fmt.Printf("Cached under %s\n\n", dir)
				for _, e := range entries {
					fmt.Printf("  %s\n", e)
				}
				return nil
			}

			m, _, cleanup, err := loadUnitFor(ctx, s, args[0])
			if err != nil {
				return err
			}
			defer cleanup()

			if len(m.MCPServers) == 0 {
				fmt.Printf("%s declares no MCP servers.\n", m.Ref())
				return nil
			}
			fmt.Printf("%s declares %d MCP server(s):\n\n", m.Ref(), len(m.MCPServers))
			for _, name := range m.MCPNames() {
				srv := m.MCPServers[name]
				src, perr := mcp.ParseSource(srv.Source)
				status := "not installed"
				if perr == nil && src.Installed(s) {
					status = "installed"
				}
				pin := "pinned"
				if perr == nil && !src.Pinned() {
					pin = "floating ref"
				}
				fmt.Printf("  %-16s %s\n", name, srv.Source)
				fmt.Printf("  %-16s %s · %s\n", "", status, pin)
				if len(srv.Tools) > 0 {
					fmt.Printf("  %-16s tools: %s\n", "", strings.Join(srv.Tools, ", "))
				}
				if len(srv.Sandbox.AllowedPaths) > 0 {
					fmt.Printf("  %-16s may read: %s\n", "", strings.Join(srv.Sandbox.AllowedPaths, ", "))
				}
				fmt.Printf("  %-16s network: %t\n", "", srv.Sandbox.Network)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

// collectCached walks the MCP cache two levels deep — kind then identity —
// which is the shape InstallDir produces.
func collectCached(dir string) ([]string, error) {
	kinds, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		sub, rerr := os.ReadDir(dir + string(os.PathSeparator) + k.Name())
		if rerr != nil {
			continue
		}
		for _, e := range sub {
			if e.IsDir() {
				out = append(out, k.Name()+"/"+e.Name())
			}
		}
	}
	return out, nil
}

func newToolsCheckCmd() *cobra.Command {
	var jsonOut, noSandbox, debug bool
	cmd := &cobra.Command{
		Use:   "check <ref|dir>",
		Short: "Start each declared server and list the tools it offers",
		Long: `Check performs the full lifecycle a run would: fetch if needed, start
each server under its sandbox, complete the MCP handshake, and list what
it offers — then shut everything down.

This is how you find out that a server needs an environment variable you
have not set, before a model is halfway through a task.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			m, srcDir, cleanup, err := loadUnitFor(ctx, s, args[0])
			if err != nil {
				return err
			}
			defer cleanup()

			if len(m.MCPServers) == 0 {
				fmt.Printf("%s declares no MCP servers.\n", m.Ref())
				return nil
			}

			// Secrets are injected exactly as they would be for a run: an
			// MCP server is usually the thing that needs the token, and a
			// check that skipped them would pass where a run fails.
			inj, err := injectFor(s, m, deviceID)
			if err != nil {
				return err
			}
			defer inj.Close()

			self, _ := os.Executable()
			mgr, err := mcp.Start(ctx, m, mcp.StartOptions{
				Store: s, WorkDir: srcDir, Env: inj.Env,
				SelfExe: self, NoSandbox: noSandbox, HomeDir: userHomeDir(),
				AutoInstall: true, Debug: debug, Logf: logf,
			})
			if err != nil {
				return err
			}
			defer mgr.Close()

			defs := mgr.Tools()
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(defs)
			}
			fmt.Printf("\n%s — %d tool(s) available to the model:\n\n", m.Ref(), len(defs))
			for _, d := range defs {
				fmt.Printf("  %-40s %s\n", d.Name, firstLineOf(d.Description))
			}
			if len(defs) == 0 {
				return fmt.Errorf("every server started, but none offered a tool the unit accepts")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&noSandbox, "no-sandbox", false, "start servers without confinement")
	cmd.Flags().BoolVar(&debug, "mcp-debug", false, "log every MCP frame")
	return cmd
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		return s[:79] + "…"
	}
	return s
}
