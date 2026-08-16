package mcp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeServerScript is a minimal MCP server in Python: it speaks the three
// methods this client uses and nothing else. Testing against a real
// subprocess rather than a mocked pipe is deliberate — the failures worth
// catching here are framing and lifecycle ones, and a mock has neither.
const fakeServerScript = `import json, sys

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

# Real servers print banners to stdout before speaking JSON-RPC.
sys.stdout.write("starting up\n")
sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    method, rid = req.get("method"), req.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {
            "protocolVersion": "2024-11-05",
            "serverInfo": {"name": "fake", "version": "0.1.0"}}})
    elif method == "notifications/initialized":
        pass
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [
            {"name": "echo", "description": "Echo the input",
             "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}},
            {"name": "boom", "description": "Always fails"},
        ]}})
    elif method == "tools/call":
        name = req["params"]["name"]
        args = req["params"].get("arguments", {})
        if name == "boom":
            send({"jsonrpc": "2.0", "id": rid, "result": {
                "content": [{"type": "text", "text": "it went wrong"}], "isError": True}})
        else:
            send({"jsonrpc": "2.0", "id": rid, "result": {
                "content": [{"type": "text", "text": "echo: " + str(args.get("text", ""))}]}})
    else:
        send({"jsonrpc": "2.0", "id": rid,
              "error": {"code": -32601, "message": "no such method " + str(method)}})
`

func python(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"python3", "python"} {
		if p, err := exec.LookPath(bin); err == nil {
			return p
		}
	}
	t.Skip("python is required for the MCP protocol tests")
	return ""
}

func startFake(t *testing.T, script string) *Client {
	t.Helper()
	py := python(t)
	dir := t.TempDir()
	path := dir + "/server.py"
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	c, err := Connect(ctx, "fake", exec.CommandContext(ctx, py, path), nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestHandshakeAndToolDiscovery(t *testing.T) {
	c := startFake(t, fakeServerScript)
	if c.ServerName != "fake" || c.ServerVersion != "0.1.0" {
		t.Errorf("server info = %q %q", c.ServerName, c.ServerVersion)
	}
	if len(c.Tools) != 2 {
		t.Fatalf("tools = %d", len(c.Tools))
	}
	// Sorted, so the offered list is stable between runs.
	if c.Tools[0].Name != "boom" || c.Tools[1].Name != "echo" {
		t.Errorf("tools not sorted: %+v", c.Tools)
	}
	if c.Tools[1].InputSchema == nil {
		t.Error("input schema was dropped")
	}
}

func TestCallReturnsText(t *testing.T) {
	c := startFake(t, fakeServerScript)
	res, err := c.Call(context.Background(), "echo", map[string]any{"text": "hi"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Error("call should not be an error")
	}
	if res.Text() != "echo: hi" {
		t.Errorf("text = %q", res.Text())
	}
}

func TestToolLevelErrorIsNotATransportError(t *testing.T) {
	// A tool that refuses is something the model can recover from; it must
	// arrive as a result, not as a Go error.
	c := startFake(t, fakeServerScript)
	res, err := c.Call(context.Background(), "boom", nil, 10*time.Second)
	if err != nil {
		t.Fatalf("a tool-level failure should not be a transport error: %v", err)
	}
	if !res.IsError {
		t.Error("IsError was not carried through")
	}
	if res.Text() != "it went wrong" {
		t.Errorf("text = %q", res.Text())
	}
}

func TestUnknownMethodSurfacesTheServersError(t *testing.T) {
	c := startFake(t, fakeServerScript)
	err := c.call(context.Background(), "nope/nope", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no such method") {
		t.Errorf("error should carry the server's message: %v", err)
	}
}

func TestHandshakeFailureIncludesServerStderr(t *testing.T) {
	// The most common real failure is a server that dies during startup
	// with a useful message on stderr. Losing that message turns every
	// such failure into an unexplained timeout.
	const script = `import sys
sys.stderr.write("ROOT_PATH does not exist\n")
sys.exit(1)
`
	py := python(t)
	dir := t.TempDir()
	path := dir + "/bad.py"
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := Connect(ctx, "bad", exec.CommandContext(ctx, py, path), nil)
	if err == nil {
		t.Fatal("expected the handshake to fail")
	}
	if !strings.Contains(err.Error(), "ROOT_PATH does not exist") {
		t.Errorf("error should include the server's stderr: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := startFake(t, fakeServerScript)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("closing twice should succeed: %v", err)
	}
	if _, err := c.Call(context.Background(), "echo", nil, time.Second); err == nil {
		t.Error("calling a closed client should fail")
	}
}

// --- source parsing -------------------------------------------------------

func TestParseSource(t *testing.T) {
	cases := []struct {
		raw                 string
		kind                string
		org, repo, ref, sub string
	}{
		{"github:modelcontextprotocol/servers#main/src/filesystem", KindGitHub,
			"modelcontextprotocol", "servers", "main", "src/filesystem"},
		{"github:org/repo", KindGitHub, "org", "repo", "main", ""},
		{"github:org/repo#v1.2.3", KindGitHub, "org", "repo", "v1.2.3", ""},
	}
	for _, tc := range cases {
		got, err := ParseSource(tc.raw)
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if got.Kind != tc.kind || got.Org != tc.org || got.Repo != tc.repo ||
			got.Ref != tc.ref || got.Path != tc.sub {
			t.Errorf("%s = %+v", tc.raw, got)
		}
	}

	for raw, kind := range map[string]string{
		"file:///opt/mcp/server":                 KindLocal,
		"npm:@modelcontextprotocol/server-files": KindNPM,
		"exec:my-mcp-server --stdio":             KindExec,
		"ghcr.io/acme/mcp-git:v1.0.0":            KindOCI,
	} {
		got, err := ParseSource(raw)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if got.Kind != kind {
			t.Errorf("%s kind = %s, want %s", raw, got.Kind, kind)
		}
	}

	for _, bad := range []string{"", "github:noslash", "file://relative/path", "npm:", "exec:", "bare"} {
		if _, err := ParseSource(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPinnedDetectsFloatingRefs(t *testing.T) {
	// A branch can change under a unit whose digest did not, which is the
	// property unit digests exist to provide.
	floating := []string{
		"github:org/repo#main",
		"github:org/repo",
		"github:org/repo#v1.2.3",
		"npm:@scope/name",
	}
	for _, raw := range floating {
		s, err := ParseSource(raw)
		if err != nil {
			t.Fatal(err)
		}
		if s.Pinned() {
			t.Errorf("%s should read as a floating ref", raw)
		}
	}

	pinned := []string{
		"github:org/repo#0123456789abcdef0123456789abcdef01234567",
		"npm:@scope/name@1.2.3",
		"file:///opt/server",
		"exec:server",
	}
	for _, raw := range pinned {
		s, err := ParseSource(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Pinned() {
			t.Errorf("%s should read as pinned", raw)
		}
	}
}
