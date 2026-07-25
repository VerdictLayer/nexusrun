package engine

import (
	"encoding/json"
	"errors"
	"fmt"
)

// This file carries the conversation and tool-calling vocabulary.
//
// Tool calling is why a unit is more than a Modelfile: a model plus a
// prompt is a chat, while a model plus a prompt plus tools it may invoke
// is an agent. It needs multi-turn state, because a tool call is a turn
// the model takes and a tool result is a turn it is answered with — the
// single Prompt/System pair the rest of the engine was built around cannot
// express either.
//
// Only backends that can carry structured tool calls natively are offered
// tools; see Capability.SupportsTools. Coaxing tool syntax out of a
// backend that has no notion of it, by asking the model to emit JSON in
// prose, is possible and unreliable, and unreliable in a way that looks
// like the unit's fault.

// Conversation roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall is a model's request to invoke one tool.
//
// Arguments stays the raw JSON the model produced, unparsed. Models
// routinely emit arguments that do not match the declared schema, and an
// executor has to be able to report exactly what was asked for rather
// than a re-serialised guess at it.
type ToolCall struct {
	// ID correlates a call with its result. Some backends assign one and
	// some do not — Ollama omits it — so a caller that needs correlation
	// must be prepared to synthesise it.
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one turn of a conversation.
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // assistant turns only

	// ToolCallID and Name identify which call a tool-result turn answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ToolDef is a tool offered to the model.
//
// Parameters is a JSON Schema object. It is carried as a decoded map
// rather than a struct because the schema is authored in the unit's
// manifest and passed through to the backend untouched: constraining it to
// a Go type here would silently drop whatever the schema uses that the
// type does not model.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Shutdowner is implemented by backends that hold a long-lived process.
// Callers that construct backends directly must call it, or the process
// outlives the command.
type Shutdowner interface{ Shutdown() }

// ShutdownAll stops every backend in the list that holds a process.
func ShutdownAll(backends []Backend) {
	for _, b := range backends {
		if s, ok := b.(Shutdowner); ok {
			s.Shutdown()
		}
	}
}

// ToolCapable returns the backends that can carry a tool-calling
// conversation, in priority order.
//
// LlamaServer leads and is present at all — All() omits it — because tool
// calling needs the chat-completions endpoint, which the one-shot CLI does
// not have. A unit that declares tools is therefore scheduled over a
// different, smaller candidate set than one that does not.
func ToolCapable() []Backend {
	return []Backend{&LlamaServer{}, &Ollama{}}
}

// errToolsUnsupported is returned by backends that cannot carry tool
// calls, rather than dropping the tools and generating anyway.
var errToolsUnsupported = errors.New(
	"this backend cannot carry tool calls: llama-cli has no chat-completions endpoint. " +
		"Install llama-server (it ships beside llama-cli) or run the unit through Ollama")

// Conversation returns the turns to send.
//
// Messages wins when set; otherwise the single-turn System/Prompt pair is
// converted into one, so every existing caller keeps working without
// knowing that conversations exist.
func (r Request) Conversation() []Message {
	if len(r.Messages) > 0 {
		return r.Messages
	}
	var msgs []Message
	if r.System != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: r.System})
	}
	if r.Prompt != "" {
		msgs = append(msgs, Message{Role: RoleUser, Content: r.Prompt})
	}
	return msgs
}

// simpleTurn flattens a conversation for a backend that can only express
// one system turn and one user turn — llama-cli's `-sys` and `-p` flags.
//
// It reports ok=false rather than dropping turns. A tool exchange
// flattened into a single prompt reads as a transcript for the model to
// continue, which produces plausible nonsense instead of an error.
func simpleTurn(msgs []Message) (system, user string, ok bool) {
	seenUser := false
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			if system != "" || seenUser {
				return "", "", false
			}
			system = m.Content
		case RoleUser:
			if seenUser {
				return "", "", false
			}
			seenUser, user = true, m.Content
		default:
			return "", "", false
		}
	}
	return system, user, true
}

// --- wire format ----------------------------------------------------------
//
// llama-server's OpenAI-compatible endpoint and Ollama's /api/chat accept
// the same envelopes for messages and tools, so both share the helpers
// below. They differ in exactly one place, noted on wireToolCall.

// toolsWire wraps tool definitions in the "function" envelope both
// backends expect.
func toolsWire(tools []ToolDef) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		// An empty schema must still be an object, or servers reject the
		// tool as malformed rather than treating it as parameterless.
		if len(t.Parameters) > 0 {
			fn["parameters"] = t.Parameters
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// messagesWire converts a conversation to the shared wire form.
func messagesWire(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		w := map[string]any{"role": m.Role}
		// Content is always present, even when empty: an assistant turn
		// that only calls tools has no text, and some servers reject the
		// message outright if the field is missing.
		w["content"] = m.Content
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				call := map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":      c.Name,
						"arguments": c.Arguments,
					},
				}
				if c.ID != "" {
					call["id"] = c.ID
				}
				calls = append(calls, call)
			}
			w["tool_calls"] = calls
		}
		if m.ToolCallID != "" {
			w["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			w["name"] = m.Name
		}
		out = append(out, w)
	}
	return out
}

// wireToolCall decodes one tool call from either dialect. This is the one
// place the two backends disagree: the OpenAI endpoint sends arguments as
// a JSON *string* holding JSON, while Ollama sends a JSON *object*.
// Normalising here keeps that difference out of every caller.
type wireToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func (w wireToolCall) toolCall() ToolCall {
	args := string(w.Function.Arguments)
	// A leading quote means the arguments arrived as an encoded string;
	// unwrap it so callers always see JSON, never JSON-in-a-string.
	if len(w.Function.Arguments) > 0 && w.Function.Arguments[0] == '"' {
		var unquoted string
		if err := json.Unmarshal(w.Function.Arguments, &unquoted); err == nil {
			args = unquoted
		}
	}
	if args == "" || args == "null" {
		args = "{}"
	}
	return ToolCall{ID: w.ID, Name: w.Function.Name, Arguments: args}
}

func toolCallsFrom(wire []wireToolCall) []ToolCall {
	if len(wire) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(wire))
	for _, w := range wire {
		out = append(out, w.toolCall())
	}
	return out
}

// Summary renders a tool call for logs and error messages.
func (c ToolCall) Summary() string { return fmt.Sprintf("%s(%s)", c.Name, c.Arguments) }
