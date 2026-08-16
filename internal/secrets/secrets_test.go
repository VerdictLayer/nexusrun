package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/store"
)

func testStore(t *testing.T) (*store.Store, *Store) {
	t.Helper()
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
	t.Setenv(MasterKeyEnv, "")
	root, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, st
}

func TestSetAndReveal(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("my-agent", "OPENAI_API_KEY", "sk-secret", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Reveal("my-agent", "OPENAI_API_KEY", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret" {
		t.Errorf("revealed %q", got)
	}
}

func TestValuesAreEncryptedOnDisk(t *testing.T) {
	root, st := testStore(t)
	if err := st.Set("my-agent", "TOKEN", "hunter2-in-the-clear", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2-in-the-clear") {
		t.Fatal("secret value is on disk in the clear")
	}
	// The key name is deliberately readable; only the value is protected.
	if !strings.Contains(string(data), "TOKEN") {
		t.Error("key name should be readable in the store")
	}
}

func TestStoreFilePermissions(t *testing.T) {
	root, st := testStore(t)
	if err := st.Set("a", "K", "v", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{Path(root), KeyPath(root)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", filepath.Base(p), perm)
		}
	}
}

func TestListNeverCarriesValues(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("a", "K", "the-value", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range st.List("") {
		if s.Value != "" || s.Previous != "" {
			t.Error("List must not carry ciphertext out of the package")
		}
	}
}

func TestDeviceScopeBeatsGlobal(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("a", "API_KEY", "global-value", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("a", "API_KEY", "kiosk-value", SetOptions{Device: "kiosk-01"}); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.Reveal("a", "API_KEY", "kiosk-01"); got != "kiosk-value" {
		t.Errorf("on kiosk-01 got %q, want the device-scoped value", got)
	}
	if got, _ := st.Reveal("a", "API_KEY", "kiosk-02"); got != "global-value" {
		t.Errorf("on an unlisted device got %q, want the global value", got)
	}
	if got, _ := st.Reveal("a", "API_KEY", ""); got != "global-value" {
		t.Errorf("with no device got %q, want the global value", got)
	}
}

func TestRotationKeepsPreviousDuringGrace(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("a", "K", "old", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Rotate("a", "K", "new", ""); err != nil {
		t.Fatal(err)
	}

	env, missing, err := st.Env("a", "", []string{"K"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if env["K"].Value != "new" {
		t.Errorf("value = %q, want the new one", env["K"].Value)
	}
	if env["K"].Previous != "old" {
		t.Errorf("previous = %q, want the old one to survive the grace period", env["K"].Previous)
	}

	// Past the grace period the old value is no longer offered.
	env, _, err = st.Env("a", "", []string{"K"}, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if env["K"].Previous != "" {
		t.Error("previous value should not be offered after the grace period")
	}
}

func TestPlainOverwriteIsNotARotation(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("a", "K", "old", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("a", "K", "corrected", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	env, _, err := st.Env("a", "", []string{"K"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if env["K"].Previous != "" {
		t.Error("a plain overwrite must not leave the old value valid — someone fixing a typo means it immediately")
	}
	if st.List("a")[0].Version != 2 {
		t.Errorf("version = %d, want 2", st.List("a")[0].Version)
	}
}

func TestExpiry(t *testing.T) {
	_, st := testStore(t)
	past := time.Now().Add(-time.Hour)
	if err := st.Set("a", "K", "v", SetOptions{Expires: &past}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Reveal("a", "K", ""); err == nil {
		t.Error("an expired secret should not be revealed")
	}
	_, missing, err := st.Env("a", "", []string{"K"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Error("an expired secret should read as missing during injection")
	}
}

func TestRemove(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("a", "K", "v", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	removed, err := st.Remove("a", "K", "")
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, _ := st.Remove("a", "K", ""); removed {
		t.Error("removing twice should report nothing removed")
	}
}

func TestKeyMustBeAnEnvVarName(t *testing.T) {
	_, st := testStore(t)
	for _, bad := range []string{"", "has-dash", "has space", "1LEADING", "has.dot"} {
		if err := st.Set("a", bad, "v", SetOptions{}); err == nil {
			t.Errorf("key %q should be rejected — it is injected as an environment variable", bad)
		}
	}
	for _, good := range []string{"OPENAI_API_KEY", "_private", "k9"} {
		if err := st.Set("a", good, "v", SetOptions{}); err != nil {
			t.Errorf("key %q should be accepted: %v", good, err)
		}
	}
}

func TestWrongMasterKeyFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEXUSRUN_HOME", dir)
	t.Setenv(MasterKeyEnv, "the right key")
	root, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("a", "K", "v", SetOptions{}); err != nil {
		t.Fatal(err)
	}

	t.Setenv(MasterKeyEnv, "a different key")
	st2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// Silently returning nothing would look like the secret was never set.
	if _, err := st2.Reveal("a", "K", ""); err == nil {
		t.Fatal("reading with the wrong master key should fail")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("a", "K1", "plaintext-alpha", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("a", "K2", "plaintext-beta", SetOptions{Device: "kiosk-01"}); err != nil {
		t.Fatal(err)
	}
	backup, err := st.Export("a", "backup-pass")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(backup), "plaintext-") {
		t.Fatal("backup contains plaintext")
	}

	// A different machine: new home, new master key.
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
	t.Setenv(MasterKeyEnv, "")
	root2, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st2, err := Open(root2)
	if err != nil {
		t.Fatal(err)
	}
	n, err := st2.Import(backup, "backup-pass")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("imported %d, want 2", n)
	}
	if got, _ := st2.Reveal("a", "K1", ""); got != "plaintext-alpha" {
		t.Errorf("K1 = %q after import", got)
	}
	if got, _ := st2.Reveal("a", "K2", "kiosk-01"); got != "plaintext-beta" {
		t.Errorf("K2 = %q after import — device scope should survive", got)
	}

	if _, err := st2.Import(backup, "wrong-pass"); err == nil {
		t.Error("importing with the wrong passphrase should fail")
	}
	if _, err := st2.Import([]byte("not a backup"), "backup-pass"); err == nil {
		t.Error("importing a non-backup should fail")
	}
}

// --- injection ------------------------------------------------------------

func unitWith(secrets []manifest.SecretRef, config []manifest.ConfigRef) *manifest.Manifest {
	return &manifest.Manifest{
		APIVersion: manifest.APIVersion, Name: "my-agent", Version: "1.0.0",
		Secrets: secrets, Config: config,
	}
}

func TestInjectBuildsEnvironment(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("my-agent", "OPENAI_API_KEY", "sk-xxx", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	m := unitWith(
		[]manifest.SecretRef{{Name: "OPENAI_API_KEY", Required: true}},
		[]manifest.ConfigRef{{Name: "MAX_RETRIES", Default: "3"}, {Name: "TIMEOUT", Default: "30", Env: "REQUEST_TIMEOUT"}},
	)
	in, err := st.Inject(m, InjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()

	want := map[string]string{
		"OPENAI_API_KEY":  "sk-xxx",
		"MAX_RETRIES":     "3",
		"REQUEST_TIMEOUT": "30",
	}
	got := map[string]string{}
	for _, kv := range in.Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (env: %v)", k, got[k], v, in.Env)
		}
	}
	if _, ok := got["TIMEOUT"]; ok {
		t.Error("config with an explicit env name should not also set its bare name")
	}
}

func TestInjectFailsOnMissingRequiredSecret(t *testing.T) {
	_, st := testStore(t)
	m := unitWith([]manifest.SecretRef{{Name: "NEEDED", Required: true}}, nil)
	_, err := st.Inject(m, InjectOptions{})
	if err == nil {
		t.Fatal("a missing required secret should fail before the agent starts")
	}
	// The error must be actionable, not just a complaint.
	for _, want := range []string{"NEEDED", "nexus secret set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestInjectToleratesMissingOptionalSecret(t *testing.T) {
	_, st := testStore(t)
	m := unitWith([]manifest.SecretRef{{Name: "OPTIONAL"}}, nil)
	in, err := st.Inject(m, InjectOptions{})
	if err != nil {
		t.Fatalf("an optional secret should not block a run: %v", err)
	}
	defer in.Close()
	if len(in.Missing) != 1 || in.Missing[0] != "OPTIONAL" {
		t.Errorf("Missing = %v", in.Missing)
	}
}

func TestMountPathStaysInsideTheMountRoot(t *testing.T) {
	_, st := testStore(t)
	if err := st.Set("my-agent", "SSL_CERT", "-----BEGIN CERT-----", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	m := unitWith([]manifest.SecretRef{
		{Name: "SSL_CERT", Required: true, MountPath: "/etc/nexus/certs/ssl.pem"},
	}, nil)

	in, err := st.Inject(m, InjectOptions{MountRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()

	if len(in.Files) != 1 {
		t.Fatalf("files = %v", in.Files)
	}
	path := in.Files[0]
	// A unit declaring /etc/... must never cause a write to /etc.
	if !strings.HasPrefix(path, root) {
		t.Fatalf("secret written to %s, outside the mount root %s", path, root)
	}
	if filepath.Base(path) != "ssl.pem" {
		t.Errorf("basename = %q, want the declared filename", filepath.Base(path))
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "-----BEGIN CERT-----" {
		t.Errorf("mounted content = %q", body)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mounted secret has mode %o, want 600", perm)
	}
	// The unit is told where the file actually landed.
	var envPath string
	for _, kv := range in.Env {
		if k, v, _ := strings.Cut(kv, "="); k == "SSL_CERT" {
			envPath = v
		}
	}
	if envPath != path {
		t.Errorf("SSL_CERT = %q, want the real path %q", envPath, path)
	}

	in.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Close should remove the mounted secret file")
	}
}

func TestAuditRecordsNamesNotValues(t *testing.T) {
	root, st := testStore(t)
	if err := st.Set("my-agent", "TOKEN", "super-secret-value", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Reveal("my-agent", "TOKEN", ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(AuditPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "super-secret-value") {
		t.Fatal("the audit log contains a secret value")
	}

	entries, err := Audit(root, "my-agent", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want set + read", len(entries))
	}
	ops := map[string]bool{}
	for _, e := range entries {
		ops[e.Op] = true
		if e.Key != "TOKEN" {
			t.Errorf("key = %q", e.Key)
		}
	}
	if !ops["set"] || !ops["read"] {
		t.Errorf("ops = %v, want both set and read", ops)
	}
}

func TestAuditFiltersByAgent(t *testing.T) {
	root, st := testStore(t)
	if err := st.Set("a", "K", "v", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("b", "K", "v", SetOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, err := Audit(root, "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Agent != "a" {
		t.Errorf("entries = %+v", entries)
	}
}
