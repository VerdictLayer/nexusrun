package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func apiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) })
}

func TestAPIRequiresTheToken(t *testing.T) {
	h := requireToken(apiHandler(), "tok123")
	cases := []struct {
		name, path, auth string
		want             int
	}{
		{"no header", "/api/runs", "", 401},
		{"wrong token", "/api/runs", "Bearer nope", 401},
		{"wrong scheme", "/api/runs", "Basic tok123", 401},
		{"right token", "/api/runs", "Bearer tok123", 200},
		{"run log without token", "/api/runs/abc", "", 401},
		{"run endpoint without token", "/api/run", "", 401},
		{"install without token", "/api/install", "", 401},
		{"console page needs none", "/", "", 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == 401 && strings.Contains(w.Body.String(), "secret") {
			t.Errorf("%s: leaked the response body", c.name)
		}
	}
}

func TestHandshakeProvesTokenWithoutRevealingIt(t *testing.T) {
	h := requireToken(apiHandler(), "tok123")
	nonce := "0123456789abcdef0123"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/handshake?nonce="+nonce, nil))
	if w.Code != 200 || w.Body.String() != Proof("tok123", nonce) {
		t.Fatalf("handshake = %d %q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tok123") {
		t.Error("handshake leaked the token")
	}
	if Proof("tok123", nonce) == Proof("other", nonce) {
		t.Error("proof must depend on the token")
	}
	for _, bad := range []string{"", "short"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/handshake?nonce="+bad, nil))
		if w.Code != 400 {
			t.Errorf("nonce %q: status %d, want 400", bad, w.Code)
		}
	}
}

func TestTokenFileIsPrivateAndRotates(t *testing.T) {
	root := t.TempDir()
	a, err := newToken(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(TokenPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %o, want 600", info.Mode().Perm())
	}
	got, err := ReadToken(root)
	if err != nil || got != a {
		t.Fatalf("ReadToken = %q, %v", got, err)
	}
	b, _ := newToken(root)
	if a == b || len(a) != 64 {
		t.Errorf("tokens must be 32 random bytes and fresh per start: %q %q", a, b)
	}

	if err := os.Chmod(TokenPath(root), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(root); err == nil {
		t.Error("a token file readable by others must be refused")
	}
}
