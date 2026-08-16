package session

import (
	"os"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := testStore(t)
	sess := New("work", "my-agent:1.0.0")
	sess.AddUser("hello")
	sess.AddAssistant("hi there", nil)
	sess.Memory = map[string]any{"k": "v"}

	if err := sess.Save(s); err != nil {
		t.Fatal(err)
	}
	back, err := Load(s, "work")
	if err != nil {
		t.Fatal(err)
	}
	if back == nil {
		t.Fatal("session not found after save")
	}
	if len(back.Messages) != 2 || back.Turns != 1 {
		t.Errorf("messages=%d turns=%d", len(back.Messages), back.Turns)
	}
	if back.Memory["k"] != "v" {
		t.Errorf("memory = %v", back.Memory)
	}
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	// `--session foo` on a fresh name starts one rather than failing.
	s := testStore(t)
	sess, err := Load(s, "never-used")
	if err != nil {
		t.Fatalf("loading an absent session should not error: %v", err)
	}
	if sess != nil {
		t.Error("expected nil for an absent session")
	}
}

func TestSessionFilePermissions(t *testing.T) {
	s := testStore(t)
	sess := New("work", "u:1")
	sess.AddUser("something private")
	if err := sess.Save(s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(PathFor(s, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode %o, want 600 — a transcript is usually the most sensitive thing on the machine", perm)
	}
}

func TestValidName(t *testing.T) {
	for _, good := range []string{"work", "a", "my-session_2.1", "A1"} {
		if err := ValidName(good); err != nil {
			t.Errorf("%q should be valid: %v", good, err)
		}
	}
	// A name is also a filename; anything that could escape the directory
	// or is unusable as one has to be rejected.
	for _, bad := range []string{"", "../escape", "has/slash", "has space", ".hidden", string(make([]byte, 65))} {
		if err := ValidName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestConversationPutsSystemFirst(t *testing.T) {
	sess := New("s", "u:1")
	sess.AddUser("hi")
	msgs := sess.Conversation("be brief")
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if msgs[0].Role != engine.RoleSystem || msgs[0].Content != "be brief" {
		t.Errorf("first turn = %+v", msgs[0])
	}
}

func TestConversationUsesTheCurrentSystemPrompt(t *testing.T) {
	// A resumed session must send the unit's *current* prompt, not the one
	// it was started with — otherwise editing a unit has no effect on a
	// conversation already in progress.
	sess := New("s", "u:1")
	sess.System = "the old prompt"
	sess.AddUser("hi")
	msgs := sess.Conversation("the new prompt")
	if msgs[0].Content != "the new prompt" {
		t.Errorf("system = %q, want the current prompt", msgs[0].Content)
	}
	// With none given, the stored one is the fallback.
	if got := sess.Conversation(""); got[0].Content != "the old prompt" {
		t.Errorf("fallback system = %q", got[0].Content)
	}
}

func TestTrimCutsAtAnExchangeBoundary(t *testing.T) {
	sess := New("s", "u:1")
	// Two complete exchanges, the first including a tool round trip.
	sess.AddUser("first")
	sess.AddAssistant("", []engine.ToolCall{{ID: "c", Name: "t", Arguments: "{}"}})
	sess.AddToolResult("c", "t", "result")
	sess.AddAssistant("answer one", nil)
	sess.AddUser("second")
	sess.AddAssistant("answer two", nil)

	dropped := sess.Trim(3)
	if dropped == 0 {
		t.Fatal("nothing was trimmed")
	}
	// Whatever survives must begin at a user turn: a tool result whose
	// assistant call was cut away is rejected by most backends.
	if sess.Messages[0].Role != engine.RoleUser {
		t.Errorf("trim left a conversation starting with %q", sess.Messages[0].Role)
	}
	for _, m := range sess.Messages {
		if m.Role == engine.RoleTool {
			t.Error("a tool result survived without its assistant turn")
		}
	}
}

func TestTrimIsANoOpWhenItWouldEmptyTheSession(t *testing.T) {
	sess := New("s", "u:1")
	sess.AddUser("only")
	sess.AddAssistant("answer", nil)
	before := len(sess.Messages)
	if dropped := sess.Trim(1); dropped != 0 {
		t.Errorf("dropped %d; trimming must not leave nothing", dropped)
	}
	if len(sess.Messages) != before {
		t.Error("messages were removed")
	}
}

func TestTrimBelowLimitDoesNothing(t *testing.T) {
	sess := New("s", "u:1")
	sess.AddUser("a")
	if dropped := sess.Trim(10); dropped != 0 {
		t.Errorf("dropped %d", dropped)
	}
	if dropped := sess.Trim(0); dropped != 0 {
		t.Errorf("a zero limit means no trimming, got %d", dropped)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s := testStore(t)
	for _, n := range []string{"one", "two", "three"} {
		sess := New(n, "u:1")
		sess.AddUser("x")
		if err := sess.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	list, err := List(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("listed %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Updated.Before(list[i].Updated) {
			t.Error("sessions are not newest first")
		}
	}
}

func TestRemove(t *testing.T) {
	s := testStore(t)
	sess := New("gone", "u:1")
	if err := sess.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := Remove(s, "gone"); err != nil {
		t.Fatal(err)
	}
	// Removing twice is a cleanup command, not an error.
	if err := Remove(s, "gone"); err != nil {
		t.Errorf("removing an absent session should succeed: %v", err)
	}
	back, _ := Load(s, "gone")
	if back != nil {
		t.Error("session still present")
	}
}

func TestSummaryUsesTheLastUserTurn(t *testing.T) {
	sess := New("s", "u:1")
	sess.AddUser("first question")
	sess.AddAssistant("answer", nil)
	sess.AddUser("second question")
	sess.AddAssistant("answer", nil)
	if got := sess.Summary(); got != "second question" {
		t.Errorf("summary = %q", got)
	}
}
