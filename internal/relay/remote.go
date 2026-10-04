package relay

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"home-harness/internal/mtls"
)

// The remote dashboard through the public gateway (internal/remoteaccess
// has the design and what this relay can and cannot do with it).
//
// A manager publishes its dashboard once and gets a path /r/<token>/,
// where the token is its relay session and TLS fingerprint sealed with
// the alias key: nothing is stored here, so the address survives relay
// restarts (as device aliases do) and needs no republishing. The gateway
// forwards only the remote dashboard's own endpoints (the page, hello,
// login, call) to the manager's relay listener, over TLS pinned to the
// sealed fingerprint, and streams replies back as they come (the event
// stream lives inside sealed calls).
//
// "hr1." tokens are a separate kind from device aliases ("ha1.", sealed
// under different additional data): a remote dashboard address is not a
// rendezvous credential, and a device alias is not a dashboard address.

const remotePrefix = "hr1."

type remotePayload struct {
	Session     string `json:"session"`
	Fingerprint string `json:"fp"`
}

// maxRemoteRequest bounds what the gateway forwards in one request body:
// remoteaccess.MaxSealedRequest plus slack.
const maxRemoteRequest = 9 << 20

func sealRemote(aead cipher.AEAD, p remotePayload) (string, error) {
	if aead == nil {
		return "", errors.New("relay: device aliases are not configured")
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(remotePrefix))
	return remotePrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openRemote(aead cipher.AEAD, token string) (remotePayload, bool) {
	if aead == nil || !strings.HasPrefix(token, remotePrefix) {
		return remotePayload{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(remotePrefix):])
	if err != nil || len(raw) < aead.NonceSize() {
		return remotePayload{}, false
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(remotePrefix))
	if err != nil {
		return remotePayload{}, false
	}
	var p remotePayload
	if json.Unmarshal(plain, &p) != nil || p.Session == "" || len(p.Fingerprint) != 64 {
		return remotePayload{}, false
	}
	return p, true
}

// publishRemote serves POST /_harness/remote-dashboards for a manager
// holding the publish token: {"session","fingerprint"} -> {"path"}.
func (g *PublicGateway) publishRemote(w http.ResponseWriter, r *http.Request) {
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if g.publishToken == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(g.publishToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Session     string `json:"session"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Session == "" || len(req.Session) > 1024 {
		http.Error(w, "invalid relay session", http.StatusBadRequest)
		return
	}
	if len(req.Fingerprint) != 64 {
		http.Error(w, "the remote dashboard needs the manager's TLS fingerprint (a manager running -insecure can't use it)", http.StatusBadRequest)
		return
	}
	token, err := sealRemote(g.server.aliasCipher, remotePayload{Session: req.Session, Fingerprint: req.Fingerprint})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"path": "/r/" + token + "/"})
}

// remoteEndpoints are the only manager paths the gateway forwards to.
var remoteEndpoints = map[string]string{"": http.MethodGet, "hello": http.MethodPost, "login": http.MethodPost, "call": http.MethodPost}

// Headers passed each way; nothing else crosses (no cookies, no
// forwarding headers a manager might mistake for its own).
var (
	remoteRequestHeaders  = []string{"Content-Type", "X-Harness-Session", "X-Harness-Seq"}
	remoteResponseHeaders = []string{"Content-Type", "Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options",
		"Referrer-Policy", "Retry-After", "X-Harness-Sealed"}
)

// remoteRedirect sends /r/<token> to /r/<token>/, so the page's relative
// addresses resolve under its own prefix.
func (g *PublicGateway) remoteRedirect(w http.ResponseWriter, r *http.Request) {
	if _, ok := openRemote(g.server.aliasCipher, r.PathValue("token")); !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, r.URL.Path+"/", http.StatusFound)
}

func (g *PublicGateway) remoteProxy(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	p, ok := openRemote(g.server.aliasCipher, token)
	method, known := remoteEndpoints[r.PathValue("rest")]
	if !ok || !known {
		http.NotFound(w, r)
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), method, "http://manager/remote/"+r.PathValue("rest"), http.MaxBytesReader(w, r.Body, maxRemoteRequest))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	out.ContentLength = r.ContentLength
	for _, h := range remoteRequestHeaders {
		if v := r.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		out.Header.Set("X-Harness-Client", host)
	}
	resp, err := g.remoteTransport(p).RoundTrip(out)
	if err != nil {
		http.Error(w, "manager unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range remoteResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	// Flush as bytes arrive: sealed replies may be long-lived streams
	// (the dashboard's live events).
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// remoteTransport is a keep-alive HTTP transport per manager: a dashboard
// opens with about ten requests at once, and the manager parks only a few
// listen registrations, so reusing connections (and retrying a connect
// that briefly finds none parked) keeps that burst from failing.
func (g *PublicGateway) remoteTransport(p remotePayload) *http.Transport {
	key := p.Session + "|" + p.Fingerprint
	g.remoteMu.Lock()
	defer g.remoteMu.Unlock()
	if t, ok := g.remoteTransports[key]; ok {
		return t
	}
	if g.remoteTransports == nil {
		g.remoteTransports = make(map[string]*http.Transport)
	}
	if len(g.remoteTransports) >= 64 {
		for k, t := range g.remoteTransports {
			t.CloseIdleConnections()
			delete(g.remoteTransports, k)
			break
		}
	}
	session, fingerprint, relayAddr := p.Session, p.Fingerprint, g.relayAddr
	t := &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			raw, err := dialConnectRetry(ctx, relayAddr, session)
			if err != nil {
				return nil, err
			}
			conn := tls.Client(raw, mtls.PinnedClientConfig(fingerprint))
			if err := conn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return conn, nil
		},
	}
	g.remoteTransports[key] = t
	return t
}

// dialConnectRetry retries a connect that found no listener parked for up
// to five seconds: the manager re-registers each slot right after it is
// used, so a burst briefly outruns the pool.
func dialConnectRetry(ctx context.Context, relayAddr, session string) (net.Conn, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := DialConnect(ctx, relayAddr, session)
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// remoteState is the gateway's per-manager connection cache.
type remoteState struct {
	remoteMu         sync.Mutex
	remoteTransports map[string]*http.Transport
}
