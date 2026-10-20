package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
)

// Protect puts the standard loopback protections in front of h: the Host and
// Origin guard, and a bearer token on every /api/ route. Other local servers
// (the Studio editor) use it so they are exactly as hard to reach from a web
// page or another account as the console is.
func Protect(h http.Handler, listenAddr, token string) http.Handler {
	return guard(requireToken(h, token), listenAddr)
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
