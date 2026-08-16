package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/runner"
	"github.com/verdictlayer/nexusrun/internal/store"
)

// NameSeparator joins a server name to a tool name.
//
// Tools are namespaced because two servers routinely offer the same name —
// a filesystem server and a git server both have "read_file" — and a model
// asked to choose between two identical names picks arbitrarily. The
// separator is a double underscore because the tool-name syntax every
// backend accepts is [A-Za-z0-9_-] and a single underscore is common
// inside real tool names.
const NameSeparator = "__"

// Manager owns the MCP servers a unit declared, for the life of one run.
type Manager struct {
	clients []*Client
	byTool  map[string]toolBinding
	logf    func(string, ...any)
}

type toolBinding struct {
	client  *Client
	tool    string
	timeout time.Duration
}

// StartOptions configures bringing servers up.
type StartOptions struct {
	Store *store.Store

	// WorkDir is the unpacked unit directory, used as each server's
	// working directory and as the default sandbox root.
	WorkDir string

	// Env is extra environment for every server, normally the unit's
	// resolved secrets — an MCP server is usually the thing that needs the
	// API token.
	Env []string

	// SelfExe and NoSandbox mirror the script runner: the sandbox is
	// applied by re-execing this binary as a helper that confines itself
	// and then becomes the server.
	SelfExe   string
	NoSandbox bool
	HomeDir   string

	// AutoInstall fetches a declared server that is not cached yet.
	AutoInstall bool

	Debug bool
	Logf  func(format string, args ...any)
}

// Start brings up every declared server and collects their tools.
//
// A server that fails to start aborts the run. The alternative — carry on
// with fewer tools — produces an agent that quietly cannot do part of its
// job, and reports success while doing it.
func Start(ctx context.Context, m *manifest.Manifest, opts StartOptions) (*Manager, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mgr := &Manager{byTool: map[string]toolBinding{}, logf: logf}
	if len(m.MCPServers) == 0 {
		return mgr, nil
	}

	debug := func(string, ...any) {}
	if opts.Debug {
		debug = logf
	}

	for _, name := range m.MCPNames() {
		srv := m.MCPServers[name]
		src, err := ParseSource(srv.Source)
		if err != nil {
			mgr.Close()
			return nil, fmt.Errorf("mcp_servers.%s: %w", name, err)
		}
		if !src.Pinned() {
			logf("warning: mcp_servers.%s tracks a floating ref (%s) — its code can change under a unit that did not", name, srv.Source)
		}
		if !src.Installed(opts.Store) {
			if !opts.AutoInstall {
				mgr.Close()
				return nil, fmt.Errorf(
					"mcp_servers.%s is not installed — run `nexus tools install %s`", name, m.Ref())
			}
			if err := src.Install(ctx, opts.Store, false, logf); err != nil {
				mgr.Close()
				return nil, fmt.Errorf("mcp_servers.%s: %w", name, err)
			}
		}

		cmd, err := serverCommand(ctx, name, src, srv, opts)
		if err != nil {
			mgr.Close()
			return nil, err
		}

		logf("starting MCP server %s (%s)", name, srv.Source)
		client, err := Connect(ctx, name, cmd, debug)
		if err != nil {
			mgr.Close()
			return nil, err
		}
		mgr.clients = append(mgr.clients, client)

		timeout := DefaultTimeout
		if srv.Timeout != "" {
			if d, perr := time.ParseDuration(srv.Timeout); perr == nil {
				timeout = d
			}
		}

		allowed := map[string]bool{}
		for _, t := range srv.Tools {
			allowed[t] = true
		}
		var offered []string
		for _, t := range client.Tools {
			if len(allowed) > 0 && !allowed[t.Name] {
				continue
			}
			full := name + NameSeparator + t.Name
			mgr.byTool[full] = toolBinding{client: client, tool: t.Name, timeout: timeout}
			offered = append(offered, full)
		}
		// A narrowing list that matches nothing is a typo, and it silently
		// produces an agent with no tools from this server.
		if len(allowed) > 0 && len(offered) == 0 {
			mgr.Close()
			return nil, fmt.Errorf(
				"mcp_servers.%s: none of the declared tools (%s) exist on that server; it offers: %s",
				name, strings.Join(srv.Tools, ", "), strings.Join(toolNames(client.Tools), ", "))
		}
		logf("  %s v%s — %d tool(s): %s",
			firstNonEmpty(client.ServerName, name), firstNonEmpty(client.ServerVersion, "?"),
			len(offered), strings.Join(offered, ", "))
	}
	return mgr, nil
}

// serverCommand builds the exec.Cmd for one server, sandbox and all.
func serverCommand(ctx context.Context, name string, src Source, srv manifest.MCPServer, opts StartOptions) (*exec.Cmd, error) {
	var argv []string
	switch {
	case len(srv.Command) > 0:
		argv = append([]string{}, srv.Command...)
	case src.Kind == KindExec:
		argv = strings.Fields(src.Location)
	default:
		return nil, fmt.Errorf(
			"mcp_servers.%s: no command given, and a %s source cannot say how to start itself — add `command:`",
			name, src.Kind)
	}

	// A relative command resolves inside the server's own directory, so a
	// cloned repo's entrypoint is named relative to the repo.
	root := src.InstallDir(opts.Store)
	if src.Kind == KindExec {
		root = opts.WorkDir
	}
	if strings.HasPrefix(argv[0], "./") || strings.HasPrefix(argv[0], "../") {
		argv[0] = filepath.Join(root, argv[0])
	}

	env := append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + root,
	}, opts.Env...)
	for k, v := range srv.Env {
		env = append(env, k+"="+v)
	}
	if lang := os.Getenv("LANG"); lang != "" {
		env = append(env, "LANG="+lang)
	}

	if !opts.NoSandbox && opts.SelfExe != "" {
		// The same helper the script runner uses: it applies the Landlock
		// policy to itself and then becomes the server, so confinement is
		// inherited and cannot be dropped.
		helper := []string{opts.SelfExe, runner.HelperCommandName(),
			"--workdir", root,
			"--home", opts.HomeDir,
			fmt.Sprintf("--network=%t", srv.Sandbox.Network),
			// A server never gets blanket home access. What it may reach is
			// exactly its allowed_paths, listed below.
			"--storage=false",
		}
		for _, p := range srv.Sandbox.AllowedPaths {
			helper = append(helper, "--read", p)
		}
		argv = append(append(helper, "--"), argv...)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = env
	return cmd, nil
}

// Tools returns every offered tool as a backend-facing definition.
func (mgr *Manager) Tools() []ToolDef {
	names := make([]string, 0, len(mgr.byTool))
	for n := range mgr.byTool {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]ToolDef, 0, len(names))
	for _, full := range names {
		b := mgr.byTool[full]
		for _, t := range b.client.Tools {
			if t.Name != b.tool {
				continue
			}
			out = append(out, ToolDef{
				Name:        full,
				Description: t.Description,
				Parameters:  t.InputSchema,
			})
		}
	}
	return out
}

// ToolDef mirrors engine.ToolDef without importing it, so package engine
// stays free of any MCP dependency.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Has reports whether a namespaced tool name belongs to a server here.
func (mgr *Manager) Has(name string) bool {
	_, ok := mgr.byTool[name]
	return ok
}

// Call invokes a namespaced tool.
func (mgr *Manager) Call(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	b, ok := mgr.byTool[name]
	if !ok {
		return "", false, fmt.Errorf("no MCP tool named %q", name)
	}
	res, err := b.client.Call(ctx, b.tool, args, b.timeout)
	if err != nil {
		return "", false, err
	}
	return res.Text(), res.IsError, nil
}

// Count is how many tools are offered.
func (mgr *Manager) Count() int { return len(mgr.byTool) }

// Close shuts every server down.
func (mgr *Manager) Close() {
	for _, c := range mgr.clients {
		_ = c.Close()
	}
	mgr.clients = nil
}

func toolNames(ts []Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
