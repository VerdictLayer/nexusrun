package main

import (
	"os"
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
