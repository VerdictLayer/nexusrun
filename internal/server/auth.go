package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The console and the warm-model daemon share one API on loopback. Loopback is
// reachable by every local user and by every web page, so the Host/Origin guard
// alone is not enough on a shared machine: any other local account could read run
// logs or spend this user's models. API calls therefore carry a bearer token that
// lives in a 0600 file in the user's NexusRun home, created fresh each time
// `nexus serve` starts.

const tokenFile = "daemon.token"

// TokenPath is where a running server publishes its token.
func TokenPath(root string) string { return filepath.Join(root, tokenFile) }

func newToken(root string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(root, ".daemon-token-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return token, os.Rename(tmp.Name(), TokenPath(root))
}

// ReadToken returns the token of the server running for this user, refusing a
// file other users can read.
func ReadToken(root string) (string, error) {
	p := TokenPath(root)
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("daemon token file is accessible by other users")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Proof is what a genuine server answers to a handshake: an HMAC of the client's
// nonce under the token. A client checks it before sending the token or a prompt,
// so a process that merely grabbed the port first learns nothing.
func Proof(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// requireToken protects every /api/ route except the handshake and any exact
// paths the caller lists as public.
func requireToken(next http.Handler, token string, public ...string) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/handshake" {
			nonce := r.URL.Query().Get("nonce")
			if len(nonce) < 16 || len(nonce) > 128 {
				http.Error(w, "nonce must be 16-128 characters", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(Proof(token, nonce)))
			return
		}
		for _, p := range public {
			if r.URL.Path == p {
				next.ServeHTTP(w, r)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="nexus"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
