package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/mcp"
)

// MCPTools adapts a running MCP manager to the Executor interface.
type MCPTools struct{ mgr *mcp.Manager }

// NewMCPTools wraps a manager. A nil manager yields an executor that owns
// nothing, so callers need no special case for a unit with no servers.
func NewMCPTools(mgr *mcp.Manager) *MCPTools { return &MCPTools{mgr: mgr} }

// Has reports whether an MCP server owns this tool.
func (m *MCPTools) Has(name string) bool {
	return m.mgr != nil && m.mgr.Has(name)
}

// Execute calls the tool on its server.
func (m *MCPTools) Execute(ctx context.Context, call engine.ToolCall) (string, bool, error) {
	if m.mgr == nil {
		return "", false, fmt.Errorf("no MCP servers are running")
	}
	args, err := ParseArgs(call.Arguments)
	if err != nil {
		// Malformed arguments are the model's mistake, and telling it so is
		// what lets it retry with valid JSON.
		return fmt.Sprintf("Error: %v", err), true, nil
	}
	out, isError, err := m.mgr.Call(ctx, call.Name, args)
	if err != nil {
		return "", false, err
	}
	if out == "" && !isError {
		out = "(the tool produced no output)"
	}
	return out, isError, nil
}

// Defs converts the manager's tools into engine definitions.
func Defs(mgr *mcp.Manager) []engine.ToolDef {
	if mgr == nil {
		return nil
	}
	src := mgr.Tools()
	out := make([]engine.ToolDef, 0, len(src))
	for _, t := range src {
		out = append(out, engine.ToolDef{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		})
	}
	return out
}

// jsonMarshal is shared by the executors.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
