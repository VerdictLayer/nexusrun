package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func home(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("NEXUSRUN_HOME", d)
	return d
}

func TestRoundTripAndPermissions(t *testing.T) {
	d := home(t)
	if err := Save("Registry.Example.com/", Entry{Token: "nxr_pat_abc", Org: "acme", Scope: "push"}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := Lookup("registry.example.com")
	if err != nil || !ok || e.Token != "nxr_pat_abc" || e.Org != "acme" {
		t.Fatalf("lookup = %+v ok=%v err=%v", e, ok, err)
	}
	info, _ := os.Stat(filepath.Join(d, "credentials.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 600", info.Mode().Perm())
	}
	if dir, _ := os.Stat(d); dir.Mode().Perm()&0o077 != 0 && dir.Mode().Perm() != 0o755 {
		// the temp dir pre-exists; only a directory we create must be 0700
	}
}

func TestDirectoryCreatedPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh", "home")
	t.Setenv("NEXUSRUN_HOME", root)
	if err := Save("r.example", Entry{Token: "t"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v err=%v, want 700", info.Mode().Perm(), err)
	}
}

func TestMissingFileAndHost(t *testing.T) {
	home(t)
	if _, ok, err := Lookup("nowhere.example"); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	_ = Save("a.example", Entry{Token: "t"})
	if _, ok, _ := Lookup("b.example"); ok {
		t.Fatal("found a login for a different host")
	}
}

func TestSecondLoginReplacesFirstAndOthersSurvive(t *testing.T) {
	home(t)
	_ = Save("a.example", Entry{Token: "one"})
	_ = Save("b.example", Entry{Token: "keep"})
	_ = Save("a.example", Entry{Token: "two"})
	if e, _, _ := Lookup("a.example"); e.Token != "two" {
		t.Errorf("a = %q", e.Token)
	}
	if e, _, _ := Lookup("b.example"); e.Token != "keep" {
		t.Errorf("b = %q", e.Token)
	}
}

func TestDelete(t *testing.T) {
	home(t)
	_ = Save("a.example", Entry{Token: "t"})
	ok, err := Delete("a.example")
	if !ok || err != nil {
		t.Fatalf("delete = %v %v", ok, err)
	}
	if ok, _ := Delete("a.example"); ok {
		t.Error("deleting twice reported success")
	}
	if _, found, _ := Lookup("a.example"); found {
		t.Error("login still present")
	}
}

func TestRefusesAWorldReadableFile(t *testing.T) {
	d := home(t)
	p := filepath.Join(d, "credentials.json")
	if err := os.WriteFile(p, []byte(`{"registries":{"a.example":{"token":"secret"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := Lookup("a.example")
	if ok || !errors.Is(err, ErrInsecure) {
		t.Fatalf("lookup of a 0644 file: ok=%v err=%v, want ErrInsecure", ok, err)
	}
	// Saving repairs it: the rewritten file is private and keeps existing entries.
	if err := Save("b.example", Entry{Token: "new"}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode after save = %o", info.Mode().Perm())
	}
	if e, ok, err := Lookup("a.example"); !ok || err != nil || e.Token != "secret" {
		t.Errorf("existing login lost: %+v ok=%v err=%v", e, ok, err)
	}
}

func TestCorruptFileIsAnErrorNotAPanic(t *testing.T) {
	d := home(t)
	_ = os.WriteFile(filepath.Join(d, "credentials.json"), []byte("{nope"), 0o600)
	if _, _, err := Lookup("a.example"); err == nil {
		t.Fatal("expected an error for corrupt JSON")
	}
}
