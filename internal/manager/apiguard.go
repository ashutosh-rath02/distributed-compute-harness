package manager

import (
	"net"
	"net/http"
	"strings"
)

// guardOperatorAPI protects the loopback operator API from the one caller
// "loopback-only" does not keep out: a web browser on this same machine.
// The phone (or desktop) running the manager also browses the web, and
// any page it visits can aim requests at http://127.0.0.1:7421:
//
//   - Cross-site request forgery: a page can fire a "simple" cross-origin
//     POST (text/plain body, no CORS preflight) at /workloads, and since
//     the handlers decode JSON regardless of Content-Type, that would run
//     an arbitrary command on every node in the fleet. Go's
//     http.CrossOriginProtection rejects non-safe requests a browser marks
//     as cross-site (Sec-Fetch-Site, falling back to Origin vs Host),
//     while harnessctl and other non-browser clients, which send neither
//     header, pass untouched.
//
//   - DNS rebinding: a page on attacker.example can re-point its own name
//     at 127.0.0.1, making its requests same-origin — so it could then read
//     GET /join-info (the permanent pairing and relay tokens) and pass the
//     check above. Rebinding only works through a DNS name, so requests are
//     accepted only when Host is an IP literal or localhost.
//
// Neither check is authentication: any local process can still use the
// API, exactly as before. It only stops a browser from being used as a
// proxy by a site the operator happened to visit.
func guardOperatorAPI(next http.Handler) http.Handler {
	csrf := http.NewCrossOriginProtection()
	protected := csrf.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !operatorHostAllowed(r.Host) {
			http.Error(w, "forbidden: the operator API only answers requests addressed to an IP address or localhost", http.StatusForbidden)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// operatorHostAllowed reports whether a request's Host header names this
// machine by something DNS rebinding cannot control: any IP literal, or
// localhost (which browsers resolve to loopback themselves, never through
// DNS). An empty Host (HTTP/1.0) is allowed, as no browser sends one.
func operatorHostAllowed(hostport string) bool {
	if hostport == "" {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if net.ParseIP(host) != nil {
		return true
	}
	return strings.EqualFold(host, "localhost")
}
