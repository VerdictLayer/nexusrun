package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConversationConvertsSingleTurnForm(t *testing.T) {
	// Every existing caller sets System and Prompt and knows nothing about
	// conversations; that form has to keep working untouched.
	got := Request{System: "be brief", Prompt: "hello"}.Conversation()
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(got), got)
	}
	if got[0].Role != RoleSystem || got[0].Content != "be brief" {
		t.Errorf("message 0 = %+v, want the system turn", got[0])
	}
	if got[1].Role != RoleUser || got[1].Content != "hello" {
		t.Errorf("message 1 = %+v, want the user turn", got[1])
	}

	// No system prompt means no system turn, not an empty one.
	if got := (Request{Prompt: "hi"}).Conversation(); len(got) != 1 || got[0].Role != RoleUser {
		t.Errorf("Conversation() = %+v, want a lone user turn", got)
	}

	// Messages wins when both are set.
	explicit := Request{
		System: "ignored", Prompt: "ignored",
		Messages: []Message{{Role: RoleUser, Content: "real"}},
	}.Conversation()
	if len(explicit) != 1 || explicit[0].Content != "real" {
		t.Errorf("Conversation() = %+v, want the explicit messages", explicit)
	}
}

// Flattening a tool exchange into one prompt makes the model continue a
// transcript, which yields plausible nonsense instead of an error.
func TestSimpleTurnRefusesWhatItCannotExpress(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		ok   bool
	}{
		{
			name: "system and user",
			msgs: []Message{{Role: RoleSystem, Content: "s"}, {Role: RoleUser, Content: "u"}},
			ok:   true,
		},
		{name: "user alone", msgs: []Message{{Role: RoleUser, Content: "u"}}, ok: true},
		{name: "empty", msgs: nil, ok: true},
		{
			name: "tool result cannot be flattened",
			msgs: []Message{{Role: RoleUser, Content: "u"}, {Role: RoleTool, Content: "42"}},
			ok:   false,
		},
		{
			name: "prior assistant turn cannot be flattened",
			msgs: []Message{{Role: RoleUser, Content: "u"}, {Role: RoleAssistant, Content: "a"}},
			ok:   false,
		},
		{
			name: "two user turns cannot be flattened",
			msgs: []Message{{Role: RoleUser, Content: "a"}, {Role: RoleUser, Content: "b"}},
			ok:   false,
		},
		{
			name: "system after user is not the CLI's shape",
			msgs: []Message{{Role: RoleUser, Content: "u"}, {Role: RoleSystem, Content: "s"}},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, ok := simpleTurn(tt.msgs); ok != tt.ok {
				t.Errorf("simpleTurn() ok = %v, want %v", ok, tt.ok)
			}
		})
	}
}

func TestToolsWireEnvelope(t *testing.T) {
	got := toolsWire([]ToolDef{{
		Name:        "search_notes",
		Description: "Search the notes",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1", len(got))
	}
	if got[0]["type"] != "function" {
		t.Errorf("type = %v, want function", got[0]["type"])
	}
	fn, ok := got[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("function envelope missing: %+v", got[0])
	}
	if fn["name"] != "search_notes" || fn["description"] != "Search the notes" {
		t.Errorf("function = %+v, want name and description carried through", fn)
	}

	// A parameterless tool still needs an object schema, or servers reject
	// it as malformed instead of treating it as taking no arguments.
	bare := toolsWire([]ToolDef{{Name: "now", Description: "current time"}})
	params, ok := bare[0]["function"].(map[string]any)["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("parameterless tool schema = %+v, want an empty object schema", params)
	}

	if toolsWire(nil) != nil {
		t.Error("toolsWire(nil) must stay nil so the field is omitted entirely")
	}
}

func TestMessagesWireCarriesToolTurns(t *testing.T) {
	got := messagesWire([]Message{
		{Role: RoleUser, Content: "what is in my notes?"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "search", Arguments: `{"q":"x"}`}}},
		{Role: RoleTool, Name: "search", ToolCallID: "call_1", Content: "nothing"},
	})
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	// An assistant turn that only calls tools has no text, and servers
	// reject the message outright when content is missing rather than empty.
	if _, ok := got[1]["content"]; !ok {
		t.Error("assistant turn omits content entirely; it must be present and empty")
	}
	calls, ok := got[1]["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %+v, want one call", got[1]["tool_calls"])
	}
	if calls[0]["id"] != "call_1" {
		t.Errorf("call id = %v, want call_1", calls[0]["id"])
	}
	if got[2]["tool_call_id"] != "call_1" || got[2]["name"] != "search" {
		t.Errorf("tool result = %+v, want it to name the call it answers", got[2])
	}
}

// The one place the two backends disagree: OpenAI sends arguments as a
// JSON string holding JSON, Ollama sends a JSON object.
func TestWireToolCallNormalisesBothDialects(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantName string
		wantArgs string
	}{
		{
			name:     "openai string arguments",
			raw:      `{"id":"call_1","function":{"name":"search","arguments":"{\"query\":\"logs\"}"}}`,
			wantName: "search",
			wantArgs: `{"query":"logs"}`,
		},
		{
			name:     "ollama object arguments",
			raw:      `{"function":{"name":"search","arguments":{"query":"logs"}}}`,
			wantName: "search",
			wantArgs: `{"query":"logs"}`,
		},
		// Absent or null arguments must become a usable empty object, not
		// something an executor has to special-case.
		{
			name:     "missing arguments",
			raw:      `{"function":{"name":"now"}}`,
			wantName: "now",
			wantArgs: `{}`,
		},
		{
			name:     "null arguments",
			raw:      `{"function":{"name":"now","arguments":null}}`,
			wantName: "now",
			wantArgs: `{}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w wireToolCall
			if err := json.Unmarshal([]byte(tt.raw), &w); err != nil {
				t.Fatal(err)
			}
			got := w.toolCall()
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Arguments != tt.wantArgs {
				t.Errorf("Arguments = %q, want %q", got.Arguments, tt.wantArgs)
			}
		})
	}
}

func TestServerRequestPicksEndpoint(t *testing.T) {
	// Tools force the chat endpoint even when Chat was not set: the
	// completion endpoint has no turn to attach a call to.
	path, body, err := serverRequest(Request{Prompt: "hi", Tools: []ToolDef{{Name: "t", Description: "d"}}}, 64)
	if err != nil {
		t.Fatalf("serverRequest: %v", err)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q, want the chat endpoint", path)
	}
	if _, ok := body["tools"]; !ok {
		t.Errorf("body omits tools: %+v", body)
	}

	// Plain completion stays on the old path, so the verified non-tool
	// behaviour is untouched.
	path, body, err = serverRequest(Request{Prompt: "hi", System: "s"}, 64)
	if err != nil {
		t.Fatalf("serverRequest: %v", err)
	}
	if path != "/completion" {
		t.Errorf("path = %q, want /completion", path)
	}
	if !strings.Contains(body["prompt"].(string), "s") {
		t.Errorf("prompt = %q, want the system text folded in", body["prompt"])
	}
	if _, ok := body["tools"]; ok {
		t.Error("completion body carries tools")
	}
}

func TestParseServerReply(t *testing.T) {
	t.Run("chat completion with tool calls", func(t *testing.T) {
		raw := []byte(`{"choices":[{"message":{"content":"",
			"tool_calls":[{"id":"call_1","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]}}],
			"usage":{"completion_tokens":7}}`)
		got, err := parseServerReply(raw)
		if err != nil {
			t.Fatalf("parseServerReply: %v", err)
		}
		if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "search" {
			t.Fatalf("ToolCalls = %+v, want one call to search", got.ToolCalls)
		}
		if got.Tokens != 7 {
			t.Errorf("Tokens = %d, want 7 from usage when timings are absent", got.Tokens)
		}
	})

	t.Run("plain completion", func(t *testing.T) {
		raw := []byte(`{"content":"hello","timings":{"predicted_n":3,"predicted_per_second":12.5}}`)
		got, err := parseServerReply(raw)
		if err != nil {
			t.Fatalf("parseServerReply: %v", err)
		}
		if got.Text != "hello" || got.Tokens != 3 || got.EvalTPS != 12.5 {
			t.Errorf("reply = %+v, want the completion fields", got)
		}
	})

	t.Run("server error is surfaced", func(t *testing.T) {
		_, err := parseServerReply([]byte(`{"error":{"message":"context shift disabled"}}`))
		if err == nil || !strings.Contains(err.Error(), "context shift disabled") {
			t.Errorf("err = %v, want the server's own message", err)
		}
	})
}

// A full round trip against a stub Ollama, which is the backend most people
// have: the request must carry tools in the shape Ollama accepts, and the
// tool calls in its reply must come back normalised.
func TestOllamaToolRoundTrip(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("request went to %q, want /api/chat", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		// Ollama's dialect: arguments as an object, and no call ID.
		w.Write([]byte(`{"message":{"role":"assistant","content":"",
			"tool_calls":[{"function":{"name":"search_notes","arguments":{"query":"invoices"}}}]},
			"eval_count":9,"eval_duration":1000000000}`))
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)

	o := &Ollama{}
	res, err := o.Generate(context.Background(), Request{
		ModelRef: "ollama:llama3.1:8b",
		Prompt:   "what invoices do I have?",
		System:   "you may search notes",
		Chat:     true,
		Tools: []ToolDef{{
			Name:        "search_notes",
			Description: "Search the user's notes",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
		}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if _, ok := gotBody["tools"]; !ok {
		t.Errorf("request omitted tools: %+v", gotBody)
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %+v, want the system and user turns", gotBody["messages"])
	}

	if len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want one", res.ToolCalls)
	}
	c := res.ToolCalls[0]
	if c.Name != "search_notes" {
		t.Errorf("call name = %q, want search_notes", c.Name)
	}
	// Object arguments are normalised to JSON text, same as OpenAI's form.
	if c.Arguments != `{"query":"invoices"}` {
		t.Errorf("Arguments = %q, want normalised JSON", c.Arguments)
	}
	if res.TokensOut != 9 {
		t.Errorf("TokensOut = %d, want 9", res.TokensOut)
	}
}

// Tools have no meaning without a turn to attach them to, so declaring any
// routes the request to the chat endpoint even when Chat was not set —
// while a request with no tools stays on the completion endpoint, which is
// the behaviour already verified end-to-end.
func TestToolsImplyChatEndpoint(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":"hi","message":{"content":"hi"},"eval_count":2}`))
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)

	o := &Ollama{}
	if _, err := o.Generate(context.Background(), Request{ModelRef: "ollama:m", Prompt: "hi"}); err != nil {
		t.Fatalf("plain completion: %v", err)
	}
	if _, err := o.Generate(context.Background(), Request{
		ModelRef: "ollama:m", Prompt: "hi",
		Tools: []ToolDef{{Name: "t", Description: "d"}},
	}); err != nil {
		t.Fatalf("tool call without Chat set: %v", err)
	}
	if want := []string{"/api/generate", "/api/chat"}; strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("endpoints = %v, want %v", paths, want)
	}

	// Same rule on the llama-server side.
	path, _, err := serverRequest(Request{Prompt: "hi", Tools: []ToolDef{{Name: "t"}}}, 64)
	if err != nil {
		t.Fatalf("serverRequest: %v", err)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q, want the chat endpoint", path)
	}
}

func TestLlamaCPPRefusesToolsAndMultiTurn(t *testing.T) {
	l := &LlamaCPP{bin: "/nonexistent/llama-cli"}

	_, err := l.Generate(context.Background(), Request{
		Prompt: "hi",
		Tools:  []ToolDef{{Name: "t", Description: "d"}},
	})
	if err == nil || !strings.Contains(err.Error(), "llama-server") {
		t.Errorf("err = %v, want a refusal naming the backend to install instead", err)
	}

	_, err = l.Generate(context.Background(), Request{Messages: []Message{
		{Role: RoleUser, Content: "u"},
		{Role: RoleTool, Content: "42"},
	}})
	if err == nil || !strings.Contains(err.Error(), "multi-turn") {
		t.Errorf("err = %v, want a refusal to flatten a multi-turn conversation", err)
	}
}

// SupportsTools is read by `nexus doctor` and the console, like the rest of
// Capability.
func TestCapabilityExposesToolSupport(t *testing.T) {
	data, err := json.Marshal(Capability{Backend: "ollama", Available: true, SupportsTools: true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got["supports_tools"]; !ok || v != true {
		t.Errorf("supports_tools = %v (present: %v), want true", v, ok)
	}
}

// The fallback path used to return the backend's most capable device
// without checking the host had one: a llama.cpp built with CUDA reports
// GPU on a machine with no NVIDIA card, and offload then fails at
// generation time, far from the reason.
func TestSelectFromFallbackStillIntersectsHardware(t *testing.T) {
	b, dev, err := SelectFrom(
		[]Backend{&stubBackend{ready("llama.cpp", "gpu", "cpu")}},
		reportWith("cpu"),
		// A preference the host cannot satisfy at all forces the fallback.
		[]string{"npu"},
	)
	if err != nil {
		t.Fatalf("SelectFrom: %v", err)
	}
	if dev != "cpu" {
		t.Errorf("device = %q, want cpu — the only class this host has", dev)
	}
	if b.Name() != "llama.cpp" {
		t.Errorf("backend = %q, want llama.cpp", b.Name())
	}

	// And when nothing intersects, say so instead of picking anyway.
	_, _, err = SelectFrom(
		[]Backend{&stubBackend{ready("acc", "npu")}},
		reportWith("cpu"),
		[]string{"gpu"},
	)
	if err == nil {
		t.Fatal("SelectFrom chose a device the host does not have")
	}
	if !strings.Contains(err.Error(), "npu") {
		t.Errorf("err = %v, want it to name what the backend claimed", err)
	}
}
