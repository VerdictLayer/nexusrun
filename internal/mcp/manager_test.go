package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/store"
)

// A missing helper path must stop the server, not start it unconfined.
func TestServerCommandRefusesToRunUnsandboxedByAccident(t *testing.T) {
	src, err := ParseSource("exec:python3 server.py")
	if err != nil {
		t.Fatal(err)
	}
	_, err = serverCommand(context.Background(), "files", src, manifest.MCPServer{Source: src.Raw}, StartOptions{Store: &store.Store{Root: t.TempDir()}, WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "sandbox helper") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	cmd, err := serverCommand(context.Background(), "files", src, manifest.MCPServer{Source: src.Raw}, StartOptions{Store: &store.Store{Root: t.TempDir()}, WorkDir: t.TempDir(), NoSandbox: true})
	if err != nil || cmd.Args[0] != "python3" {
		t.Fatalf("explicit --no-sandbox: cmd = %v, err = %v", cmd, err)
	}
}
