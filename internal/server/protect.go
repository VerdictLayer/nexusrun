package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Protect puts the standard loopback protections in front of h: the Host and
// Origin guard, and a bearer token on every /api/ route. Other local servers
// (the Studio editor) use it so they are exactly as hard to reach from a web
// page or another account as the console is.
func Protect(h http.Handler, listenAddr, token string) http.Handler {
	return guard(requireToken(h, token), listenAddr)
}

// ProtectExcept is Protect, except that the exact paths in public skip the token
// check. They are still behind the Host and Origin guard. It exists for a
// one-time code exchange, the only call that has to work before the page has a
// token.
func ProtectExcept(h http.Handler, listenAddr, token string, public ...string) http.Handler {
	return guard(requireToken(h, token, public...), listenAddr)
}

// ProtectAll is Protect for a server whose every route is an API: the token is
// required on every path, not only under /api/. The fleet endpoint uses it,
// because OpenAI-style clients call /v1/... paths.
func ProtectAll(h http.Handler, listenAddr, token string) http.Handler {
	want := []byte(token)
	return guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}), listenAddr)
}

// NewSessionToken returns a random token that lives only in memory, for servers
// that are tied to one process and need no file.
func NewSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RequireLoopback refuses any listen address that is not a loopback one. The
// guard and token assume the only callers are on this machine.
func RequireLoopback(addr string) error {
	if host, _, err := net.SplitHostPort(addr); err != nil || !isLoopbackHost(host) {
		return fmt.Errorf("refusing to listen on %q: this server is for this machine only and binds to a loopback address (for example 127.0.0.1:7717)", addr)
	}
	return nil
}
