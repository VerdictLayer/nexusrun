package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAppWindowUsesAnInstalledChromiumBrowserWithoutTheToken(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("looks the browser up on PATH, as on Linux")
	}
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "chromium"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	exe := findAppBrowser()
	if exe != filepath.Join(bin, "chromium") {
		t.Fatalf("findAppBrowser = %q", exe)
	}
	if err := openAppWindow(exe, "http://127.0.0.1:7718/#code=abc123"); err != nil {
		t.Fatal(err)
	}
	var got string
	for i := 0; i < 50 && got == ""; i++ {
		b, _ := os.ReadFile(argsFile)
		got = strings.TrimSpace(string(b))
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, "--app=http://127.0.0.1:7718/#code=abc123") || strings.Contains(got, "token") {
		t.Errorf("args = %q", got)
	}
}

func TestNoChromiumBrowserMeansNoAppWindow(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("looks the browser up on PATH, as on Linux")
	}
	t.Setenv("PATH", t.TempDir())
	if exe := findAppBrowser(); exe != "" {
		t.Errorf("found %q on an empty PATH", exe)
	}
}

func TestDesktopQuoteFollowsTheDesktopEntrySpec(t *testing.T) {
	for in, want := range map[string]string{
		"/home/me/bin/nexus":  `"/home/me/bin/nexus"`,
		"/a b/nexus":          `"/a b/nexus"`,
		`say "hi"`:            `"say \\"hi\\""`,
		"a$b":                 `"a\\$b"`,
		"a`b":                 "\"a\\\\`b\"",
		`back\slash`:          `"back\\\\slash"`,
		"100%":                `"100%%"`,
		"/home/me/My Flows/x": `"/home/me/My Flows/x"`,
	} {
		if got := desktopQuote(in); got != want {
			t.Errorf("desktopQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestLauncherInstallsAndRemovesAValidMenuEntry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the application menu entry is for Linux")
	}
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	ws := filepath.Join(t.TempDir(), "My Workflows")
	if err := launcherCommand(ws, false); err != nil {
		t.Fatal(err)
	}
	desktop := filepath.Join(data, "applications", "nexusrun-studio.desktop")
	icon := filepath.Join(data, "icons", "hicolor", "scalable", "apps", "nexusrun-studio.svg")
	b, err := os.ReadFile(desktop)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"Type=Application", "Name=NexusRun Studio", "Terminal=false", "Icon=nexusrun-studio",
		" studio --app --quit-when-closed --workspace \"" + ws + "\"\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("desktop entry lacks %q:\n%s", want, got)
		}
	}
	if svg, err := os.ReadFile(icon); err != nil || !strings.HasPrefix(string(svg), "<svg") {
		t.Errorf("icon: %v", err)
	}
	if p, err := exec.LookPath("desktop-file-validate"); err == nil {
		if out, err := exec.Command(p, desktop).CombinedOutput(); err != nil {
			t.Errorf("desktop-file-validate: %v\n%s", err, out)
		}
	}
	if err := launcherCommand("", true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{desktop, icon} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s should have been removed", p)
		}
	}
	if err := launcherCommand("", true); err != nil {
		t.Errorf("removing twice must not fail: %v", err)
	}
}
