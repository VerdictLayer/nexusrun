package fleet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/verdictlayer/nexusrun/internal/agent"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/server"
)

const (
	defaultMaxTokens = 512
	maxMaxTokens     = 4096
	maxRequestBytes  = 1 << 20
)

type apiKey struct {
	Key string `json:"key"`
}

func apiKeyPath(home string) string { return filepath.Join(Root(home), "api.key") }

// APIKey returns the key local applications use to call `nexus fleet serve`,
// creating it on first use. It stays the same across restarts so an
// application configured once keeps working.
func APIKey(home string) (string, error) {
	var k apiKey
	err := readPrivate(apiKeyPath(home), &k)
	if err == nil && strings.HasPrefix(k.Key, "nxf_") && len(k.Key) == 47 {
		return k.Key, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil {
		return "", errors.New("invalid fleet API key file")
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	k.Key = "nxf_" + base64.RawURLEncoding.EncodeToString(b)
	if err = writePrivate(apiKeyPath(home), k); err != nil {
		return "", err
	}
	return k.Key, nil
}

// chatter is what the endpoint needs from an opened release.
type chatter interface {
	Chat(ctx context.Context, msgs []engine.Message, maxTokens int, stream func(string)) (*agent.Result, error)
	Close()
}

// Endpoint answers OpenAI-style chat requests with the active release. It keeps
// that release's model loaded between requests and switches to a new release
// on the first request after it activates.
type Endpoint struct {
	home string

	// mu serializes generations, and is also held while a candidate is
	// tested, so a test never shares memory with a loaded release.
	mu      sync.Mutex
	testing atomic.Bool
	current chatter
	release *Release
	ref     string

	active func() (*Release, error)
	open   func(context.Context, Release) (chatter, string, error)
}

func NewEndpoint(home string) *Endpoint {
	return &Endpoint{
		home:   home,
		active: func() (*Release, error) { return activeRelease(home) },
		open: func(ctx context.Context, r Release) (chatter, string, error) {
			a, err := OpenAgent(ctx, home, r)
			if err != nil {
				return nil, "", err
			}
			return a, a.Manifest.Ref(), nil
		},
	}
}

// Prepare wraps a candidate test so it runs with the loaded release unloaded
// and requests turned away until it finishes.
func (ep *Endpoint) Prepare(prepare Prepare) Prepare {
	return func(ctx context.Context, e Enrollment, r Release) Outcome {
		ep.testing.Store(true)
		defer ep.testing.Store(false)
		ep.mu.Lock()
		defer ep.mu.Unlock()
		ep.unload()
		return prepare(ctx, e, r)
	}
}

func (ep *Endpoint) unload() {
	if ep.current != nil {
		ep.current.Close()
	}
	ep.current, ep.release, ep.ref = nil, nil, ""
}

// Close unloads the model.
func (ep *Endpoint) Close() {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.unload()
}

// load returns the active release, opening it if it is not the one loaded.
// Called with mu held.
func (ep *Endpoint) load(ctx context.Context) (chatter, *Release, string, error) {
	r, err := ep.active()
	if err != nil {
		return nil, nil, "", err
	}
	if ep.current != nil && ep.release.ID == r.ID {
		return ep.current, ep.release, ep.ref, nil
	}
	ep.unload()
	c, ref, err := ep.open(ctx, *r)
	if err != nil {
		return nil, nil, "", err
	}
	ep.current, ep.release, ep.ref = c, r, ref
	return c, r, ref, nil
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func fail(w http.ResponseWriter, status int, kind, msg string) {
	var e apiError
	e.Error.Message, e.Error.Type = msg, kind
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

func (ep *Endpoint) unavailable(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoActive) {
		fail(w, http.StatusServiceUnavailable, "unavailable", "no release is active on this host yet")
		return
	}
	fail(w, http.StatusServiceUnavailable, "unavailable", "the active release could not be loaded: "+err.Error())
}

func (ep *Endpoint) busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "30")
	fail(w, http.StatusServiceUnavailable, "unavailable", "an update is being tested on this host; retry shortly")
}

// Handler serves /v1/models and /v1/chat/completions. It carries no
// protection of its own; Serve puts it behind the loopback guard and key.
func (ep *Endpoint) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", ep.models)
	mux.HandleFunc("POST /v1/chat/completions", ep.chat)
	return mux
}

func (ep *Endpoint) models(w http.ResponseWriter, r *http.Request) {
	if ep.testing.Load() {
		ep.busy(w)
		return
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	_, rel, ref, err := ep.load(r.Context())
	if err != nil {
		ep.unavailable(w, err)
		return
	}
	created, _ := time.Parse(time.RFC3339, rel.CreatedAt)
	writeJSON(w, rel, map[string]any{"object": "list", "data": []any{map[string]any{"id": ref, "object": "model", "created": created.Unix(), "owned_by": "nexusrun"}}})
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Stream              bool            `json:"stream"`
	Tools               json.RawMessage `json:"tools"`
}

// text accepts a string or an array of text parts, the two forms clients send.
func (m chatMessage) text() (string, error) {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return "", errors.New("message content must be a string or a list of text parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported; send text", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func parseChat(r *http.Request) ([]engine.Message, int, bool, error) {
	var req chatRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes+1))
	if err := dec.Decode(&req); err != nil {
		return nil, 0, false, errors.New("request body must be a JSON chat completion request")
	}
	if t := strings.TrimSpace(string(req.Tools)); t != "" && t != "null" && t != "[]" {
		return nil, 0, false, errors.New("this endpoint runs the release's own tools; requests cannot add tools")
	}
	var msgs []engine.Message
	users := 0
	for _, m := range req.Messages {
		switch m.Role {
		case engine.RoleSystem, engine.RoleUser, engine.RoleAssistant:
		case "developer":
			m.Role = engine.RoleSystem
		default:
			return nil, 0, false, fmt.Errorf("message role %q is not supported", m.Role)
		}
		text, err := m.text()
		if err != nil {
			return nil, 0, false, err
		}
		if m.Role == engine.RoleUser {
			users++
		}
		msgs = append(msgs, engine.Message{Role: m.Role, Content: text})
	}
	if users == 0 {
		return nil, 0, false, errors.New("messages must include a user message")
	}
	n := req.MaxCompletionTokens
	if n == 0 {
		n = req.MaxTokens
	}
	if n < 0 {
		return nil, 0, false, errors.New("max_tokens must be positive")
	}
	if n == 0 {
		n = defaultMaxTokens
	}
	return msgs, min(n, maxMaxTokens), req.Stream, nil
}

func completionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "chatcmpl-" + hex.EncodeToString(b)
}

// writeJSON names the release that answered in headers, so an application can
// log exactly which tested version it was talking to.
func writeJSON(w http.ResponseWriter, rel *Release, v any) {
	w.Header().Set("X-Nexus-Release", rel.ID)
	w.Header().Set("X-Nexus-Digest", rel.Digest)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (ep *Endpoint) chat(w http.ResponseWriter, r *http.Request) {
	msgs, maxTokens, stream, err := parseChat(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if ep.testing.Load() {
		ep.busy(w)
		return
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	c, rel, ref, err := ep.load(r.Context())
	if err != nil {
		ep.unavailable(w, err)
		return
	}
	id, created := completionID(), time.Now().Unix()
	if !stream {
		res, err := c.Chat(r.Context(), msgs, maxTokens, nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		writeJSON(w, rel, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": ref, "system_fingerprint": rel.Digest,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": res.Text}, "finish_reason": "stop"}},
			"usage":   map[string]any{"completion_tokens": res.TokensOut},
		})
		return
	}
	flusher, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("X-Nexus-Release", rel.ID)
	h.Set("X-Nexus-Digest", rel.Digest)
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	event := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": ref, "system_fingerprint": rel.Digest,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	event(chunk(map[string]any{"role": "assistant"}, nil))
	_, err = c.Chat(r.Context(), msgs, maxTokens, func(s string) { event(chunk(map[string]any{"content": s}, nil)) })
	if err != nil {
		var e apiError
		e.Error.Message, e.Error.Type = err.Error(), "server_error"
		event(e)
	} else {
		event(chunk(map[string]any{}, "stop"))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ServeOptions configures Serve.
type ServeOptions struct {
	Addr     string
	Interval time.Duration
	Timeout  time.Duration
	Control  Control
	Logf     func(format string, args ...any)
}

// Serve keeps this host's release current and answers local applications with
// it. It holds the fleet lock for its whole life, so it is the one process
// updating and running releases from this home.
func Serve(ctx context.Context, home string, opts ServeOptions) error {
	if err := server.RequireLoopback(opts.Addr); err != nil {
		return err
	}
	unlock, err := Lock(home)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err = LoadEnrollment(home); err != nil {
		return errors.New("enroll this host with nexus fleet join first")
	}
	key, err := APIKey(home)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return err
	}
	ep := NewEndpoint(home)
	defer ep.Close()
	srv := &http.Server{Handler: server.ProtectAll(ep.Handler(), opts.Addr, key), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	opts.Logf("serving the active release at http://%s/v1 (API key: nexus fleet key)", opts.Addr)

	prepare := ep.Prepare(NativePrepare(home))
	// Log changes, not every poll: a host that stays current is quiet.
	var last string
	for {
		op, cancel := context.WithTimeout(ctx, opts.Timeout)
		result, syncErr := syncLocked(op, home, HostHardware(), opts.Control, prepare)
		cancel()
		line := strings.TrimSpace(result.Status + " " + result.ReleaseID)
		if syncErr != nil {
			line += ": " + syncErr.Error()
		}
		if line != last {
			opts.Logf("sync: %s", line)
			last = line
		}
		timer := time.NewTimer(opts.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = srv.Shutdown(shutdown)
			return nil
		case err := <-served:
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}
