package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/verdictlayer/nexusrun/internal/server"
)

func TestSafeLoginLink(t *testing.T) {
	for _, ok := range []string{
		"https://app.nexusrun.dev/?device=ABCD-EFGH",
		"http://127.0.0.1:8080/?activate=1",
		"http://localhost:3000/",
	} {
		if !safeLoginLink(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{
		"",
		"file:///etc/passwd",
		"C:\\Windows\\System32\\calc.exe",
		"javascript:alert(1)",
		"ms-msdt:/id PCWDiagnostic",
		"http://evil.example/",
		"https://user:pw@evil.example/",
		"https://app.nexusrun.dev/\x1b]0;owned\x07",
		"//evil.example/",
	} {
		if safeLoginLink(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestVerifyDaemonRejectsImpostors(t *testing.T) {
	const token = "real-token"
	genuine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(server.Proof(token, r.URL.Query().Get("nonce"))))
	}))
	defer genuine.Close()
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(server.Proof("guessed", r.URL.Query().Get("nonce"))))
	}))
	defer impostor.Close()
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer silent.Close()

	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	ctx := context.Background()
	if !verifyDaemon(ctx, host(genuine), token) {
		t.Error("a server that holds the token must verify")
	}
	if verifyDaemon(ctx, host(impostor), token) {
		t.Error("a server without the token must not verify")
	}
	if verifyDaemon(ctx, host(silent), token) {
		t.Error("a server that sends no proof must not verify")
	}
	if verifyDaemon(ctx, "127.0.0.1:1", token) {
		t.Error("nothing listening must not verify")
	}
}
