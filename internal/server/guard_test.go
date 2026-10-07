package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// run sends one request through the guard to a handler that answers 200, and
// reports the status. host overrides the Host header (a rebound name arrives
// with its own).
func run(t *testing.T, bind, method, host string, hdr map[string]string) (int, http.Header) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(method, "http://"+host+"/api/run", strings.NewReader(`{"unit":"x"}`))
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	guard(next, bind).ServeHTTP(rec, req)
	return rec.Code, rec.Header()
}

var js = map[string]string{"Content-Type": "application/json"}

func with(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range js {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestHostHeaderBlocksDNSRebinding(t *testing.T) {
	const bind = "127.0.0.1:7717"
	cases := []struct {
		host string
		want int
	}{
		{"127.0.0.1:7717", 200},
		{"localhost:7717", 200},
		{"LOCALHOST:7717", 200},
		{"[::1]:7717", 200},
		{"attacker.example:7717", 403},           // a name rebound to 127.0.0.1
		{"127.0.0.1.attacker.example:7717", 403}, // look-alike
		{"localhost.attacker.example:7717", 403},
		{"127.0.0.1:9999", 403}, // some other local service's port
		{"127.0.0.1", 403},      // no port
		{"", 403},
	}
	for _, c := range cases {
		if got, _ := run(t, bind, http.MethodGet, c.host, nil); got != c.want {
			t.Errorf("GET Host %q = %d, want %d", c.host, got, c.want)
		}
	}
}

func TestRebindingCannotPostEvenWithMatchingOrigin(t *testing.T) {
	// After rebinding, the page is same-origin to attacker.example, so Origin matches Host
	// and the content type can be JSON. Only the Host check stops it.
	got, _ := run(t, "127.0.0.1:7717", http.MethodPost, "attacker.example:7717",
		with(map[string]string{"Origin": "http://attacker.example:7717", "Sec-Fetch-Site": "same-origin"}))
	if got != 403 {
		t.Fatalf("rebound POST = %d, want 403", got)
	}
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	const bind, host = "127.0.0.1:7717", "127.0.0.1:7717"
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"curl or the CLI: no Origin, no Fetch-Metadata", with(nil), 200},
		{"the console itself", with(map[string]string{"Origin": "http://127.0.0.1:7717", "Sec-Fetch-Site": "same-origin"}), 200},
		{"address bar / extension: Sec-Fetch-Site none", with(map[string]string{"Sec-Fetch-Site": "none"}), 200},
		{"another site", with(map[string]string{"Origin": "https://attacker.example"}), 403},
		{"Origin null (sandboxed iframe, file://)", with(map[string]string{"Origin": "null"}), 403},
		{"cross-site Fetch-Metadata", with(map[string]string{"Sec-Fetch-Site": "cross-site"}), 403},
		{"same-site (a sibling subdomain) is not enough", with(map[string]string{"Sec-Fetch-Site": "same-site"}), 403},
		{"the same host on another port is another origin", with(map[string]string{"Origin": "http://127.0.0.1:9999"}), 403},
		{"no-preflight text/plain body", map[string]string{"Content-Type": "text/plain"}, 415},
		{"form post", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 415},
		{"no content type", nil, 415},
	}
	for _, c := range cases {
		if got, _ := run(t, bind, http.MethodPost, host, c.hdr); got != c.want {
			t.Errorf("%s: POST = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPreflightIsNeverApproved(t *testing.T) {
	got, hdr := run(t, "127.0.0.1:7717", http.MethodOptions, "127.0.0.1:7717",
		map[string]string{"Origin": "https://attacker.example", "Access-Control-Request-Method": "POST"})
	if got == 200 {
		t.Fatalf("a cross-origin preflight was answered 200")
	}
	if hdr.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS headers must never be set")
	}
}

func TestExplicitNonLoopbackBindKeepsOriginChecks(t *testing.T) {
	const bind = "0.0.0.0:7717"
	if got, _ := run(t, bind, http.MethodGet, "kiosk.lan:7717", nil); got != 200 {
		t.Errorf("a deliberate LAN bind must accept its own hostname, got %d", got)
	}
	if got, _ := run(t, bind, http.MethodPost, "kiosk.lan:7717", with(map[string]string{"Origin": "https://attacker.example"})); got != 403 {
		t.Errorf("cross-origin write on a LAN bind = %d, want 403", got)
	}
	if got, _ := run(t, bind, http.MethodPost, "kiosk.lan:7717", with(map[string]string{"Origin": "http://kiosk.lan:7717"})); got != 200 {
		t.Errorf("same-origin write on a LAN bind = %d, want 200", got)
	}
}

func TestResponseHeaders(t *testing.T) {
	_, h := run(t, "127.0.0.1:7717", http.MethodGet, "127.0.0.1:7717", nil)
	for k, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"Cache-Control":           "no-store",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
