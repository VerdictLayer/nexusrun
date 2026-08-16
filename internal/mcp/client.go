// Package mcp speaks the Model Context Protocol over stdio.
//
// MCP is how an agent reaches tools it did not ship with — a filesystem
// server, a Git server, a company's internal one. NexusRun treats those
// servers the way it treats models: declared in the unit, version-pinned,
// resolved into a local cache, and started under the same kernel sandbox as
// everything else. The alternative that MCP tooling usually assumes — the
// user installs and runs the servers themselves, and the agent trusts
// whatever is listening — is exactly the arrangement a portable artifact
// exists to avoid.
//
// Only the stdio transport is implemented. It is the simplest and the only
// one that keeps a server inside the sandbox: an SSE server is a network
// endpoint someone else controls, which is a different trust decision and
// deserves to be made explicitly rather than inherited from a URL in a YAML
// file.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the MCP revision this client negotiates.
const ProtocolVersion = "2024-11-05"

// DefaultTimeout bounds a single request.
const DefaultTimeout = 30 * time.Second

// Tool is a tool a server offers.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// Content is one piece of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallResult is a tool call's outcome.
type CallResult struct {
	Content []Content `json:"content"`

	// IsError marks a tool-level failure — the call reached the tool and
	// the tool said no. It is distinct from a transport error, and it is
	// fed back to the model rather than aborting the run: "that path does
	// not exist" is something a model can recover from.
	IsError bool `json:"isError,omitempty"`
}

// Text flattens a result's content into a string for the model.
func (r CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// --- JSON-RPC -------------------------------------------------------------

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// Client is a connection to one MCP server process.
type Client struct {
	name string
	cmd  *exec.Cmd

	mu     sync.Mutex
	stdin  io.WriteCloser
	nextID int64

	// pending correlates responses to waiting callers. A server may
	// interleave notifications and responses freely, so replies cannot be
	// assumed to arrive in request order.
	pending map[int64]chan response

	readErr error
	closed  bool

	ServerName    string
	ServerVersion string
	Tools         []Tool

	// Debug, when set, receives every frame in both directions.
	Debug func(format string, args ...any)
}

// Connect launches a server process and completes the MCP handshake.
func Connect(ctx context.Context, name string, cmd *exec.Cmd, debug func(string, ...any)) (*Client, error) {
	if debug == nil {
		debug = func(string, ...any) {}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// The server's stderr is its log. Capturing it is what turns "the
	// health check timed out" into "the server said ENOENT on ROOT_PATH".
	var stderr strings.Builder
	cmd.Stderr = &lockedWriter{w: &stderr}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mcp server %s: %w", name, err)
	}

	c := &Client{
		name: name, cmd: cmd, stdin: stdin,
		pending: map[int64]chan response{}, Debug: debug,
	}
	go c.readLoop(stdout)

	if err := c.handshake(ctx); err != nil {
		c.Close()
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%w\n  %s said: %s", err, name, tailLines(detail, 5))
		}
		return nil, err
	}
	return c, nil
}

func (c *Client) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	// Tool results carry file contents; the 64 KB default is far too small.
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		c.Debug("[%s] ← %s", c.name, truncate(line, 400))

		var resp response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			// Servers routinely print banners to stdout before speaking
			// JSON-RPC. Skipping unparseable lines is more robust than
			// treating the first one as a protocol failure.
			continue
		}
		if resp.ID == nil {
			continue // a notification; nothing is waiting on it
		}
		c.mu.Lock()
		ch, ok := c.pending[*resp.ID]
		delete(c.pending, *resp.ID)
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}

	c.mu.Lock()
	c.readErr = sc.Err()
	if c.readErr == nil {
		c.readErr = errors.New("server closed its output")
	}
	// Wake everyone still waiting, or they block until their context expires.
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("mcp server %s is closed", c.name)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	body, err := json.Marshal(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	c.Debug("[%s] → %s", c.name, truncate(string(body), 400))

	c.mu.Lock()
	_, werr := c.stdin.Write(append(body, '\n'))
	c.mu.Unlock()
	if werr != nil {
		return fmt.Errorf("write to mcp server %s: %w", c.name, werr)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			c.mu.Lock()
			rerr := c.readErr
			c.mu.Unlock()
			return fmt.Errorf("mcp server %s stopped responding: %v", c.name, rerr)
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}
}

func (c *Client) notify(method string, params any) error {
	body, err := json.Marshal(request{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	c.Debug("[%s] → %s", c.name, truncate(string(body), 200))
	c.mu.Lock()
	defer c.mu.Unlock()
	_, werr := c.stdin.Write(append(body, '\n'))
	return werr
}

func (c *Client) handshake(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"clientInfo":      map[string]any{"name": "nexusrun", "version": "1"},
	}, &init)
	if err != nil {
		return fmt.Errorf("mcp handshake with %s failed: %w", c.name, err)
	}
	c.ServerName, c.ServerVersion = init.ServerInfo.Name, init.ServerInfo.Version

	// The spec requires this notification before any other request.
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	return c.refreshTools(ctx)
}

func (c *Client) refreshTools(ctx context.Context) error {
	var list struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &list); err != nil {
		return fmt.Errorf("list tools from %s: %w", c.name, err)
	}
	sort.SliceStable(list.Tools, func(i, j int) bool { return list.Tools[i].Name < list.Tools[j].Name })
	c.Tools = list.Tools
	return nil
}

// Call invokes a tool.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any, timeout time.Duration) (*CallResult, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if args == nil {
		args = map[string]any{}
	}
	var res CallResult
	if err := c.call(ctx, "tools/call", map[string]any{
		"name": tool, "arguments": args,
	}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Name is the server's declared name in the unit.
func (c *Client) Name() string { return c.name }

// Close shuts the server down.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	stdin := c.stdin
	c.mu.Unlock()

	// Closing stdin is the protocol's shutdown signal; a well-behaved
	// server exits on EOF. Kill is the fallback for one that does not.
	if stdin != nil {
		_ = stdin.Close()
	}
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-done
	}
	return nil
}

// lockedWriter serialises writes to a strings.Builder that the child
// process writes to from another goroutine.
type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n  ")
}
