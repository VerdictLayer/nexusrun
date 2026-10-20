//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
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

// Landlock cannot stop UDP, and older kernels cannot stop TCP either, so the
// seccomp filter is what makes "no network capability" true. The policy is
// irreversible, so it is observed from a child process.
func TestApplyDeniesAllInternetSocketsWithoutNetworkCapability(t *testing.T) {
	if !Supported() {
		t.Skip("kernel lacks Landlock")
	}
	if os.Getenv("NEXUSRUN_SANDBOX_CHILD") == "1" {
		allow := os.Getenv("NEXUSRUN_SANDBOX_NET") == "1"
		caps := []string(nil)
		if allow {
			caps = []string{"network"}
		}
		if err := Apply(FromCapabilities(caps, os.Getenv("NEXUSRUN_SANDBOX_DIR"), "")); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		for _, c := range []struct {
			name         string
			domain, kind int
		}{
			{"tcp4", unix.AF_INET, unix.SOCK_STREAM},
			{"udp4", unix.AF_INET, unix.SOCK_DGRAM},
			{"tcp6", unix.AF_INET6, unix.SOCK_STREAM},
			{"udp6", unix.AF_INET6, unix.SOCK_DGRAM},
			{"raw", unix.AF_INET, unix.SOCK_RAW},
			{"packet", unix.AF_PACKET, unix.SOCK_DGRAM},
		} {
			fd, err := unix.Socket(c.domain, c.kind, 0)
			if fd >= 0 {
				unix.Close(fd)
			}
			if allow {
				// A raw or packet socket needs privilege the test lacks; only the
				// ones an ordinary process may open prove the filter is absent.
				if (c.name == "raw" || c.name == "packet") || err == nil {
					continue
				}
				if err == unix.EACCES {
					t.Errorf("%s socket denied although the network capability was granted", c.name)
				}
				continue
			}
			if err != unix.EACCES {
				t.Errorf("%s socket: got %v, want EACCES", c.name, err)
			}
		}
		// Local sockets still work: interpreters and tools rely on them.
		if !allow {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatalf("AF_UNIX socketpair denied: %v", err)
			}
			unix.Close(fds[0])
			unix.Close(fds[1])
		}
		return
	}

	for _, net := range []string{"0", "1"} {
		cmd := exec.Command(os.Args[0], "-test.run=TestApplyDeniesAllInternetSocketsWithoutNetworkCapability", "-test.v")
		cmd.Env = append(os.Environ(),
			"NEXUSRUN_SANDBOX_CHILD=1",
			"NEXUSRUN_SANDBOX_NET="+net,
			"NEXUSRUN_SANDBOX_DIR="+t.TempDir(),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sandboxed child (network=%s) failed: %v\n%s", net, err, out)
		}
	}
}
