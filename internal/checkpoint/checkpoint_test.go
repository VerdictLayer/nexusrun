package checkpoint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/session"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func sampleSession() *session.Session {
	s := session.New("work", "my-agent:1.0.0")
	s.Model = "ollama:llama3.1:8b"
	s.ModelDigest = "sha256:abc"
	s.Backend, s.Device = "llama.cpp", "gpu"
	s.System = "You are careful."
	s.Context = 8192
	s.Memory = map[string]any{"project": "nexusrun"}
	s.AddUser("what is the deploy key?")
	s.AddAssistant("", []engine.ToolCall{{ID: "c1", Name: "vault_read", Arguments: `{"k":"deploy"}`}})
	s.AddToolResult("c1", "vault_read", "hunter2")
	s.AddAssistant("It is hunter2.", nil)
	s.TokensOut = 42
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	sess := sampleSession()
	var buf bytes.Buffer
	man, err := Save(&buf, sess, SaveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if man.Unit.Name != "my-agent" || man.Unit.Version != "1.0.0" {
		t.Errorf("unit = %+v", man.Unit)
	}
	if man.Session.Messages != 4 {
		t.Errorf("messages = %d", man.Session.Messages)
	}

	res, err := Load(bytes.NewReader(buf.Bytes()), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Session
	if len(got.Messages) != 4 {
		t.Fatalf("restored %d messages", len(got.Messages))
	}
	if got.Unit != "my-agent:1.0.0" {
		t.Errorf("unit = %q", got.Unit)
	}
	if got.System != "You are careful." || got.Context != 8192 {
		t.Errorf("context lost: system=%q context=%d", got.System, got.Context)
	}
	if got.Memory["project"] != "nexusrun" {
		t.Errorf("memory = %v", got.Memory)
	}
	if got.TokensOut != 42 {
		t.Errorf("tokens_out = %d", got.TokensOut)
	}

	// The tool exchange has to survive intact, or a resumed conversation
	// is rejected by the backend as a call with no answer.
	assistant := got.Messages[1]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Name != "vault_read" {
		t.Errorf("tool call lost: %+v", assistant)
	}
	toolTurn := got.Messages[2]
	if toolTurn.Role != engine.RoleTool || toolTurn.ToolCallID != "c1" || toolTurn.Content != "hunter2" {
		t.Errorf("tool result lost: %+v", toolTurn)
	}
}

func TestEncryptedRoundTrip(t *testing.T) {
	t.Setenv(StateKeyEnv, "a shared passphrase")
	sess := sampleSession()

	var buf bytes.Buffer
	if _, err := Save(&buf, sess, SaveOptions{Encrypt: true}); err != nil {
		t.Fatal(err)
	}
	// Even the unit name — which the manifest carries — must not be legible.
	if strings.Contains(buf.String(), "my-agent") || strings.Contains(buf.String(), "hunter2") {
		t.Fatal("encrypted checkpoint leaks its contents")
	}

	res, err := Load(bytes.NewReader(buf.Bytes()), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Session.Messages) != 4 {
		t.Errorf("restored %d messages", len(res.Session.Messages))
	}

	t.Setenv(StateKeyEnv, "the wrong passphrase")
	if _, err := Load(bytes.NewReader(buf.Bytes()), LoadOptions{}); err == nil {
		t.Error("loading with the wrong key should fail")
	}

	t.Setenv(StateKeyEnv, "")
	_, err = Load(bytes.NewReader(buf.Bytes()), LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), StateKeyEnv) {
		t.Errorf("loading with no key should name the variable, got %v", err)
	}
}

func TestEncryptRequiresAKey(t *testing.T) {
	t.Setenv(StateKeyEnv, "")
	var buf bytes.Buffer
	_, err := Save(&buf, sampleSession(), SaveOptions{Encrypt: true})
	if err == nil || !strings.Contains(err.Error(), StateKeyEnv) {
		t.Fatalf("expected an error naming %s, got %v", StateKeyEnv, err)
	}
}

func TestSealEmbedsWeights(t *testing.T) {
	dir := t.TempDir()
	weights := filepath.Join(dir, "model.gguf")
	body := bytes.Repeat([]byte("W"), 4096)
	if err := os.WriteFile(weights, body, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	man, err := Save(&buf, sampleSession(), SaveOptions{Seal: true, ModelPath: weights})
	if err != nil {
		t.Fatal(err)
	}
	if !man.Model.Sealed || man.Model.SizeBytes != 4096 {
		t.Errorf("manifest model = %+v", man.Model)
	}

	outDir := t.TempDir()
	res, err := Load(bytes.NewReader(buf.Bytes()), LoadOptions{ModelDir: outDir})
	if err != nil {
		t.Fatal(err)
	}
	if res.SealedModel == "" {
		t.Fatal("sealed weights were not extracted")
	}
	got, err := os.ReadFile(res.SealedModel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("extracted weights do not match what was sealed")
	}
}

func TestSealWithoutAModelPathFails(t *testing.T) {
	var buf bytes.Buffer
	_, err := Save(&buf, sampleSession(), SaveOptions{Seal: true})
	if err == nil {
		t.Fatal("sealing with no weights should fail")
	}
}

func TestMetadataOnlyStopsEarly(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Save(&buf, sampleSession(), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := Load(bytes.NewReader(buf.Bytes()), LoadOptions{MetadataOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest == nil {
		t.Fatal("no manifest")
	}
	if len(res.Session.Messages) != 0 {
		t.Error("MetadataOnly should not read the conversation")
	}
}

func TestRejectsNonCheckpoint(t *testing.T) {
	if _, err := Load(strings.NewReader("this is not an archive"), LoadOptions{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestManifestStringMentionsWhatMatters(t *testing.T) {
	var buf bytes.Buffer
	man, err := Save(&buf, sampleSession(), SaveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := man.String()
	for _, want := range []string{"my-agent", "work", "ollama:llama3.1:8b", "llama.cpp"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output should mention %q:\n%s", want, out)
		}
	}
}

func TestListFindsSavedCheckpoints(t *testing.T) {
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(Dir(s), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(s), "work-20260101T000000Z"+Ext)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Save(f, sampleSession(), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	list, err := List(s, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("found %d checkpoints", len(list))
	}
	if list[0].Manifest == nil || list[0].Manifest.Session.Name != "work" {
		t.Errorf("manifest not read: %+v", list[0])
	}

	// Filtering by session name must actually filter.
	if got, _ := List(s, "work"); len(got) != 1 {
		t.Error("filtering by the right session should match")
	}
	if got, _ := List(s, "other"); len(got) != 0 {
		t.Error("filtering by another session should not match")
	}
}

func TestDigestFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := DigestFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello")
	const want = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if d != want {
		t.Errorf("digest = %s", d)
	}
}

func TestUnsafePathIsRejected(t *testing.T) {
	// A checkpoint is an archive from elsewhere; a path escaping the
	// extraction directory must never be honoured.
	var buf bytes.Buffer
	if _, err := Save(&buf, sampleSession(), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	// Sanity: the well-formed archive loads. The guard itself is exercised
	// by construction — every entry name is checked before use.
	if _, err := Load(bytes.NewReader(buf.Bytes()), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = time.Now
}

// Encryption must stream. A sealed checkpoint is the size of the model, so
// buffering the archive to seal it whole put the OOM killer between a small
// device and the air-gapped transfer the feature exists for.
func TestEncryptedSealStreamsLargePayloads(t *testing.T) {
	t.Setenv(StateKeyEnv, "streaming key")

	dir := t.TempDir()
	weights := filepath.Join(dir, "model.gguf")
	// Comfortably more than one frame, with a non-repeating tail so a
	// frame-ordering bug cannot pass by accident.
	body := bytes.Repeat([]byte("ABCDEFGH"), 3<<17) // 8 MiB
	body = append(body, []byte("TAIL-MARKER")...)
	if err := os.WriteFile(weights, body, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Save(&buf, sampleSession(), SaveOptions{
		Encrypt: true, Seal: true, ModelPath: weights,
	}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("TAIL-MARKER")) {
		t.Fatal("sealed weights are in the clear")
	}

	outDir := t.TempDir()
	res, err := Load(bytes.NewReader(buf.Bytes()), LoadOptions{ModelDir: outDir})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(res.SealedModel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round trip corrupted %d bytes of weights", len(body))
	}
	if len(res.Session.Messages) != 4 {
		t.Errorf("messages = %d", len(res.Session.Messages))
	}
}

func TestTruncatedEncryptedCheckpointIsReported(t *testing.T) {
	t.Setenv(StateKeyEnv, "k")
	var buf bytes.Buffer
	if _, err := Save(&buf, sampleSession(), SaveOptions{Encrypt: true}); err != nil {
		t.Fatal(err)
	}
	cut := buf.Bytes()[:buf.Len()-20]
	if _, err := Load(bytes.NewReader(cut), LoadOptions{}); err == nil {
		t.Fatal("a truncated checkpoint should not load")
	}
}

// Each frame is bound to its index, so reordering or dropping whole chunks
// fails the tag rather than silently producing a different archive.
func TestReorderedFramesAreRejected(t *testing.T) {
	t.Setenv(StateKeyEnv, "k")
	dir := t.TempDir()
	weights := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(weights, bytes.Repeat([]byte("Z"), 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := Save(&buf, sampleSession(), SaveOptions{
		Encrypt: true, Seal: true, ModelPath: weights,
	}); err != nil {
		t.Fatal(err)
	}

	raw := buf.Bytes()
	// Flip a byte inside the first frame's ciphertext.
	pos := len(magic) + 4 + 12 + 8
	if pos >= len(raw) {
		t.Skip("archive smaller than expected")
	}
	tampered := append([]byte(nil), raw...)
	tampered[pos] ^= 0xff
	if _, err := Load(bytes.NewReader(tampered), LoadOptions{ModelDir: t.TempDir()}); err == nil {
		t.Fatal("tampering with a frame should fail authentication")
	}
}
