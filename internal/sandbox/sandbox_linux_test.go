//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"testing"
)

func TestExistingFiltersMissingPaths(t *testing.T) {
	got := existing("/dev/null", "/nonexistent-xyzzy", "/dev/zero")
	want := []string{"/dev/null", "/dev/zero"}
	if len(got) != len(want) {
		t.Fatalf("existing() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("existing() = %v, want %v", got, want)
		}
	}
}

// Apply is irreversible and inherited across exec, so enforcement can
// only be observed from a child process. Landlock previously granted no
// access to /dev at all, which broke `cmd 2>/dev/null` and any runtime
// that seeds its RNG from /dev/urandom — that is, almost every script.
func TestApplyPermitsStandardDevices(t *testing.T) {
	if !Supported() {
		t.Skip("kernel lacks Landlock")
	}
	if os.Getenv("NEXUSRUN_SANDBOX_CHILD") == "1" {
		dir := os.Getenv("NEXUSRUN_SANDBOX_DIR")
		if err := Apply(FromCapabilities(nil, dir, "")); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// Writable discard sink.
		f, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("/dev/null not writable under policy: %v", err)
		}
		f.Close()
		// Readable entropy source.
		g, err := os.Open("/dev/urandom")
		if err != nil {
			t.Fatalf("/dev/urandom not readable under policy: %v", err)
		}
		g.Close()
		// The confinement itself must still hold.
		if err := os.WriteFile("/etc/nexusrun-escape-probe", []byte("x"), 0o644); err == nil {
			os.Remove("/etc/nexusrun-escape-probe")
			t.Fatal("wrote outside the unit directory: policy is not confining")
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestApplyPermitsStandardDevices", "-test.v")
	cmd.Env = append(os.Environ(),
		"NEXUSRUN_SANDBOX_CHILD=1",
		"NEXUSRUN_SANDBOX_DIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed child failed: %v\n%s", err, out)
	}
}

func TestClassifySeparatesFilesFromDirs(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/weights.gguf"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs, files := classify([]string{dir, file, "/nonexistent-xyzzy", ""})
	if len(dirs) != 1 || dirs[0] != dir {
		t.Errorf("dirs = %v", dirs)
	}
	if len(files) != 1 || files[0] != file {
		t.Errorf("files = %v", files)
	}
}

// A read-only grant is routinely a single file — a model's weights, or a
// mounted secret. Applying directory rights to one is rejected by the
// kernel with "inconsistent access rights", which failed the entire
// ruleset and so failed the whole run.
func TestApplyGrantsReadOnlyFiles(t *testing.T) {
	if !Supported() {
		t.Skip("kernel lacks Landlock")
	}
	if os.Getenv("NEXUSRUN_SANDBOX_CHILD") == "1" {
		work := os.Getenv("NEXUSRUN_SANDBOX_DIR")
		secret := os.Getenv("NEXUSRUN_SANDBOX_FILE")
		if err := Apply(FromCapabilities(nil, work, "", secret)); err != nil {
			t.Fatalf("Apply with a file read-grant: %v", err)
		}
		body, err := os.ReadFile(secret)
		if err != nil {
			t.Fatalf("granted file not readable under policy: %v", err)
		}
		if string(body) != "cert-body" {
			t.Fatalf("read %q", body)
		}
		// The grant is read-only, and everything else still denied.
		if err := os.WriteFile(secret, []byte("no"), 0o600); err == nil {
			t.Fatal("a read-only grant allowed a write")
		}
		return
	}

	// The file must live outside the writable work directory, or the
	// RWDirs rule would grant it and the test would prove nothing.
	secretDir := t.TempDir()
	secret := secretDir + "/ssl.pem"
	if err := os.WriteFile(secret, []byte("cert-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestApplyGrantsReadOnlyFiles", "-test.v")
	cmd.Env = append(os.Environ(),
		"NEXUSRUN_SANDBOX_CHILD=1",
		"NEXUSRUN_SANDBOX_DIR="+t.TempDir(),
		"NEXUSRUN_SANDBOX_FILE="+secret,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed child failed: %v\n%s", err, out)
	}
}
