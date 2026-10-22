package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientDoesNotForwardCredentialsToRedirects(t *testing.T) {
	requests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.Write([]byte(`{}`)) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := NewClient(strings.TrimPrefix(source.URL, "http://")+"/acme/agent", "nxr_host_"+strings.Repeat("x", 43))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Checkin(context.Background(), Checkin{Hardware: hw}); err == nil {
		t.Fatal("followed credential redirect")
	}
	if requests != 0 {
		t.Fatal("forwarded a fleet request to another origin")
	}
}

func TestClientValidatesAssignmentsAndRedactsRemoteErrors(t *testing.T) {
	for _, repo := range []string{"https://registry.test/acme/agent", "evil@registry.test/acme/agent", "registry.test/acme/../private", "registry.test/acme/agent:latest", "registry.test/acme/agent?token=secret"} {
		if _, err := NewClient(repo, "secret"); err == nil {
			t.Fatalf("accepted %s", repo)
		}
	}
	for _, repo := range []string{"registry.test/acme/agent", "localhost:1234/acme/agent", "127.0.0.1:1234/acme/agent"} {
		c, err := NewClient(repo, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(repo, "registry.test") && !strings.HasPrefix(c.Base, "https://") {
			t.Fatal("external registry used plaintext")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"secret /private/path"}`))
	}))
	defer server.Close()
	c, _ := NewClient(strings.TrimPrefix(server.URL, "http://")+"/acme/agent", "secret")
	_, err := c.Checkin(context.Background(), Checkin{Hardware: hw})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/private/path") {
		t.Fatal("remote body leaked into an error")
	}
}
