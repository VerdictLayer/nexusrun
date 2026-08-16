// Package session holds an agent's conversation across runs.
//
// Until now every `nexus run` was one shot: a prompt in, an answer out,
// nothing kept. That is the right default for a CLI, and it is also why
// there was nothing to checkpoint — an agent with no memory has no state to
// move between machines.
//
// A session is that memory: an ordered conversation, the model and unit it
// belongs to, and whatever the agent chose to remember alongside it. It is
// stored as plain JSON, because the thing most worth having when an agent
// misbehaves is the ability to read exactly what it was told.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/store"
)

// FormatVersion identifies the on-disk shape.
const FormatVersion = "nexusrun.dev/session/v1"

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Session is one agent's continuing conversation.
type Session struct {
	Version string `json:"version"`
	Name    string `json:"name"`

	Unit       string `json:"unit"`
	UnitDigest string `json:"unit_digest,omitempty"`

	Model       string `json:"model,omitempty"`
	ModelDigest string `json:"model_digest,omitempty"`

	// Backend and Device record where the last turn actually ran. They are
	// advisory on restore: a checkpoint moved to a machine with different
	// hardware still resumes, on whatever that machine has.
	Backend string `json:"backend,omitempty"`
	Device  string `json:"device,omitempty"`

	System   string           `json:"system,omitempty"`
	Messages []engine.Message `json:"messages"`

	// Memory is free-form state the agent keeps beside the transcript.
	// The runtime never interprets it; it only carries it, which is what
	// makes it useful to a script unit that wants somewhere durable to put
	// something without inventing its own file.
	Memory map[string]any `json:"memory,omitempty"`

	// Context records the window the conversation was built for, so a
	// restore onto a smaller model can say so rather than silently truncate.
	Context int `json:"context,omitempty"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Turns   int       `json:"turns"`

	// TokensIn and TokensOut accumulate across the whole session.
	TokensOut int `json:"tokens_out,omitempty"`
}

// New starts a session.
func New(name, unit string) *Session {
	now := time.Now().UTC()
	return &Session{
		Version: FormatVersion, Name: name, Unit: unit,
		Created: now, Updated: now,
	}
}

// Dir is where sessions live.
func Dir(s *store.Store) string { return filepath.Join(s.Root, "sessions") }

// PathFor returns a session's file.
func PathFor(s *store.Store, name string) string {
	return filepath.Join(Dir(s), name+".json")
}

// ValidName checks a session name is usable as a filename.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf(
			"session name %q must be 1–64 characters of letters, digits, dot, dash or underscore — it is also a filename",
			name)
	}
	return nil
}

// Load reads a session, returning nil (no error) when it does not exist:
// `--session foo` on a name that has never been used starts one.
func Load(s *store.Store, name string) (*Session, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(PathFor(s, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("read session %s: %w", name, err)
	}
	if sess.Version != FormatVersion {
		return nil, fmt.Errorf("session %s is format %q, this build reads %q",
			name, sess.Version, FormatVersion)
	}
	return &sess, nil
}

// Save writes a session atomically.
func (sess *Session) Save(s *store.Store) error {
	if err := ValidName(sess.Name); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(s), 0o755); err != nil {
		return err
	}
	sess.Version = FormatVersion
	sess.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	path := PathFor(s, sess.Name)
	tmp := path + ".tmp"
	// 0600: a transcript is often the most sensitive thing on the machine.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove deletes a session.
func Remove(s *store.Store, name string) error {
	err := os.Remove(PathFor(s, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List returns every session, most recently used first.
func List(s *store.Store) ([]*Session, error) {
	entries, err := os.ReadDir(Dir(s))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		sess, err := Load(s, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || sess == nil {
			continue
		}
		out = append(out, sess)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// --- conversation ---------------------------------------------------------

// Conversation returns the turns to send to a backend, with the system
// prompt first. The system turn is stored separately rather than as the
// first message so that changing a unit's prompt changes what a resumed
// session sends, instead of replaying the prompt it was started with.
func (sess *Session) Conversation(system string) []engine.Message {
	if system == "" {
		system = sess.System
	}
	msgs := make([]engine.Message, 0, len(sess.Messages)+1)
	if system != "" {
		msgs = append(msgs, engine.Message{Role: engine.RoleSystem, Content: system})
	}
	return append(msgs, sess.Messages...)
}

// AddUser appends a user turn.
func (sess *Session) AddUser(content string) {
	sess.Messages = append(sess.Messages, engine.Message{Role: engine.RoleUser, Content: content})
}

// AddAssistant appends an assistant turn, including any tool calls it made.
func (sess *Session) AddAssistant(content string, calls []engine.ToolCall) {
	sess.Messages = append(sess.Messages,
		engine.Message{Role: engine.RoleAssistant, Content: content, ToolCalls: calls})
	sess.Turns++
}

// AddToolResult appends the answer to one tool call.
func (sess *Session) AddToolResult(callID, name, content string) {
	sess.Messages = append(sess.Messages, engine.Message{
		Role: engine.RoleTool, Content: content, ToolCallID: callID, Name: name,
	})
}

// Trim keeps the most recent n messages, preserving tool-call integrity.
//
// A conversation that grows past the context window has to lose something,
// and the naive cut — drop the oldest n messages — routinely severs an
// assistant turn from the tool results answering it, which most backends
// reject outright. This drops whole exchanges from the front instead.
func (sess *Session) Trim(maxMessages int) int {
	if maxMessages <= 0 || len(sess.Messages) <= maxMessages {
		return 0
	}
	cut := len(sess.Messages) - maxMessages
	// Advance the cut to the next user turn, so what remains starts at the
	// beginning of an exchange rather than in the middle of one.
	for cut < len(sess.Messages) && sess.Messages[cut].Role != engine.RoleUser {
		cut++
	}
	if cut >= len(sess.Messages) {
		return 0 // trimming would leave nothing; keep the conversation whole
	}
	sess.Messages = append([]engine.Message(nil), sess.Messages[cut:]...)
	return cut
}

// Summary renders a one-line description for listings.
func (sess *Session) Summary() string {
	last := ""
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		if sess.Messages[i].Role == engine.RoleUser {
			last = oneLine(sess.Messages[i].Content, 48)
			break
		}
	}
	return last
}

func oneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
