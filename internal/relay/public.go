package relay

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"home-harness/internal/mtls"
)

type publicEnrollment struct {
	Token            string    `json:"token"`
	ExpiresAt        time.Time `json:"expiresAt"`
	Fingerprint      string    `json:"fingerprint"`
	Insecure         bool      `json:"insecure"`
	Session          string    `json:"-"`
	RequestedSession string    `json:"session"`
}

// PublicGateway exposes internet-facing enrollment URLs and proxies only
// valid token-scoped requests through the rendezvous path to the manager.
type PublicGateway struct {
	server       *Server
	relayAddr    string
	mu           sync.Mutex
	entries      map[string]publicEnrollment
	publishToken string
	remoteState  // the remote dashboard's connections (remote.go)
}

func NewPublicGateway(server *Server, relayAddr, publishToken string) *PublicGateway {
	return &PublicGateway{server: server, relayAddr: relayAddr, publishToken: publishToken, entries: make(map[string]publicEnrollment)}
}

func (g *PublicGateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_harness/enrollments", g.publish)
	mux.HandleFunc("GET /enroll/{token}", g.proxy)
	mux.HandleFunc("GET /enroll/{token}/{rest...}", g.proxy)
	// The remote dashboard (remote.go).
	mux.HandleFunc("POST /_harness/remote-dashboards", g.publishRemote)
	mux.HandleFunc("GET /r/{token}", g.remoteRedirect)
	mux.HandleFunc("/r/{token}/{rest...}", g.remoteProxy)
	return mux
}

func (g *PublicGateway) publish(w http.ResponseWriter, r *http.Request) {
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if g.publishToken == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(g.publishToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var e publicEnrollment
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&e); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	session := e.RequestedSession
	if session == "" || len(session) > 1024 {
		http.Error(w, "invalid relay session", http.StatusBadRequest)
		return
	}
	if len(e.Token) != 64 || !e.ExpiresAt.After(time.Now()) || e.ExpiresAt.After(time.Now().Add(time.Hour)) {
		http.Error(w, "invalid enrollment", http.StatusBadRequest)
		return
	}
	if !e.Insecure && len(e.Fingerprint) != 64 {
		http.Error(w, "invalid manager fingerprint", http.StatusBadRequest)
		return
	}
	credential, err := g.server.issueAlias(session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	e.Session = session
	g.mu.Lock()
	for token, old := range g.entries {
		if !old.ExpiresAt.After(time.Now()) {
			delete(g.entries, token)
		}
	}
	if len(g.entries) >= 4096 {
		g.mu.Unlock()
		http.Error(w, "too many active enrollments", http.StatusServiceUnavailable)
		return
	}
	g.entries[e.Token] = e
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"relayCredential": credential})
}

func (g *PublicGateway) proxy(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	g.mu.Lock()
	e, ok := g.entries[token]
	if ok && !e.ExpiresAt.After(time.Now()) {
		delete(g.entries, token)
		ok = false
	}
	g.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	raw, err := DialConnect(ctx, g.relayAddr, e.Session)
	if err != nil {
		http.Error(w, "manager unavailable", http.StatusBadGateway)
		return
	}
	conn := net.Conn(raw)
	if !e.Insecure {
		conn = tls.Client(raw, mtls.PinnedClientConfig(e.Fingerprint))
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
	req := r.Clone(ctx)
	req.URL.Scheme, req.URL.Host, req.RequestURI = "http", "manager", ""
	req.Host = "manager"
	resp, err := transport.RoundTrip(req)
	if err != nil {
		conn.Close()
		http.Error(w, "manager request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
