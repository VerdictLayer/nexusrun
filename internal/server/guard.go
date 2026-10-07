package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guard protects the local API from web pages the user happens to have open.
//
// A browser on this machine can reach loopback services, so "the request came
// from loopback" proves nothing: a page on any site can send requests to
// 127.0.0.1, and a site that re-points its own DNS name at 127.0.0.1 (DNS
// rebinding) makes those requests look same-origin. The guard therefore checks
// what a browser cannot forge:
//
//   - Host must be this server's own address (loopback binds only). A rebound
//     name arrives with its own Host header and is refused.
//   - State-changing requests must be same-origin: Sec-Fetch-Site, when sent,
//     must be same-origin or none, and Origin, when sent, must match Host.
//     curl and the NexusRun CLI send neither header, so they are unaffected.
//   - State-changing requests must be application/json, which a cross-site
//     form or no-cors fetch cannot send without a preflight (and no CORS
//     headers are ever set, so the preflight fails).
func guard(next http.Handler, listenAddr string) http.Handler {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host, port = listenAddr, ""
	}
	loopbackBind := isLoopbackHost(host)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}

		if loopbackBind && !hostAllowed(r.Host, port) {
			http.Error(w, "forbidden: unexpected Host header", http.StatusForbidden)
			return
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
			return
		}

		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
				return
			}
		}
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// hostAllowed accepts only a loopback name on this server's own port.
func hostAllowed(hostHeader, port string) bool {
	h, p, err := net.SplitHostPort(hostHeader)
	if err != nil {
		return false // no port: not us (the console always runs on an explicit port)
	}
	return (port == "" || p == port) && isLoopbackHost(h)
}
