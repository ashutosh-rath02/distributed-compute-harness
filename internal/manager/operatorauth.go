package manager

import (
	"context"
	"net"
	"time"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"home-harness/internal/domain"
)

// Operator authentication: who may drive the operator API at all.
//
// Loopback was never a real boundary on the phone that hosts the manager:
// Android shares 127.0.0.1 across every installed app, so without this any
// app with network permission could submit workloads (code on every node),
// revoke nodes, or read GET /join-info's permanent tokens. guardOperatorAPI
// (apiguard.go) stops browsers being used as a proxy, but deliberately lets
// non-browser clients through — which is exactly what such an app is.
//
// The credential is a random token in a file in the manager's private
// state directory. It is accepted only as an Authorization: Bearer header,
// in one of two forms:
//
//   - the raw token, for harnessctl and scripts that can read the file;
//   - the dashboard session, an HMAC of the token (dashboardSession), which
//     POST /login hands out in exchange for the token and the dashboard
//     keeps in localStorage.
//
// Never a cookie. Cookies are not isolated by port (RFC 6265 §8.5, and
// SameSite ignores port too), so a cookie set by 127.0.0.1:7421 would also
// be sent to a malicious app listening on 127.0.0.1:8888 the moment it
// made the browser open its URL. localStorage is scoped by scheme, host,
// and port, and a header is only ever sent by the dashboard's own code.
//
// The session is deterministic, so it survives manager restarts (phones
// reboot) and every session dies together when the token is rotated
// (delete the file and restart). Signing out of the dashboard only drops
// that browser's copy; it cannot revoke a session value leaked elsewhere.
// The dashboard must therefore stay free of external scripts — it holds a
// bearer credential in JS-readable storage.

const dashboardSessionContext = "harness-dashboard-session-v1"

// dashboardSession derives the dashboard's bearer value from the operator
// token, so the raw token itself never has to live in browser storage.
func dashboardSession(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(dashboardSessionContext))
	return hex.EncodeToString(mac.Sum(nil))
}

// LoadOrCreateOperatorToken returns the token stored at path, creating the
// file with a fresh random token (owner-only permissions) if it doesn't
// exist yet — the same load-or-create shape as the manager's TLS cert.
func LoadOrCreateOperatorToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 32 {
			return "", fmt.Errorf("operator token file %q is too short to be a real token; delete it to generate a new one", path)
		}
		return token, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read operator token %q: %w", path, err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create operator token dir: %w", err)
		}
	}
	// O_EXCL: two managers racing to create it must not end up with
	// different tokens on disk and in memory.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create operator token %q: %w", path, err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return token, f.Close()
}

// operatorPublic reports whether r may reach the operator API without a
// credential. Kept to the minimum the dashboard needs to bootstrap:
//   - GET / and GET /live — the dashboard and live-view shells, static
//     HTML with no data in them;
//   - POST /login — exchanging the token for a session;
//   - GET /server-proof — proves this manager holds the token (it takes
//     no credential and returns none);
//   - GET /version — which build this is (version, commit, platform), no
//     fleet data and no secrets;
//   - GET /enrollments/{token}/qr — loaded by an <img>, which can't send
//     headers; its path already contains the unguessable invitation token,
//     so knowing the URL already means knowing the secret.
func operatorPublic(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case (path == "/" || path == "/live") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		return true
	case path == "/login" && r.Method == http.MethodPost:
		return true
	case path == "/server-proof" && r.Method == http.MethodGet:
		return true
	// Liveness and readiness probes (observe.go): a fixed word each, no
	// data, so an orchestrator or load balancer can ask without a token.
	case (path == "/healthz" || path == "/readyz") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		return true
	case path == "/version" && r.Method == http.MethodGet:
		return true // build identity only: version, commit, platform
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/enrollments/") &&
		strings.HasSuffix(path, "/qr") && strings.Count(path, "/") == 3:
		return true
	}
	return false
}

func bearerToken(r *http.Request) string {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// requireOperator enforces operator authentication on every route except
// operatorPublic ones, then the caller's role against routeRoles (roles.go).
// A Server with no OperatorToken (tests, embedding) is unauthenticated;
// cmd/manager always configures one.
func (s *Server) requireOperator(next http.Handler) http.Handler {
	mux, _ := next.(*http.ServeMux)
	// Remote calls (remote.go) were verified by the remote handler and are
	// held to the remote mode by remoteGate; local requests pass through it.
	next = s.remoteGate(next)
	if s.cfg.OperatorToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if remoteCallFrom(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		if operatorPublic(r) {
			next.ServeHTTP(w, r)
			return
		}
		pattern := ""
		if mux != nil {
			_, pattern = mux.Handler(r)
		}
		if pattern != "POST /artifacts" && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)
		}
		got := bearerToken(r)
		p, ok := s.authenticate(got, time.Now())
		if !ok {
			// The metrics token opens only /metrics, /healthz and /readyz
			// (observe.go), so a scraper never holds the operator token.
			if mt := s.cfg.MetricsToken; mt != "" && got != "" && observePath(r) && subtle.ConstantTimeCompare([]byte(got), []byte(mt)) == 1 {
				next.ServeHTTP(w, r.WithContext(withActor(r.Context(), actorMetricsToken)))
				return
			}
			// The AI key opens only the OpenAI-compatible API (openai.go).
			if isOpenAIPath(r.URL.Path) {
				if aiKey := s.aiKey(); aiKey != "" && subtle.ConstantTimeCompare([]byte(got), []byte(aiKey)) == 1 {
					next.ServeHTTP(w, r.WithContext(withActor(r.Context(), actorAIKey)))
					return
				}
			}
			if got != "" && s.authFail.tooMany(authSource(r), time.Now()) {
				w.Header().Set("Retry-After", "60")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": "too many failed sign-in attempts from this source: wait a minute, then use the right token. A correct token still works.",
				})
				return
			}
			if isOpenAIPath(r.URL.Path) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="home-harness AI API"`)
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
					"use the manager's AI key as the API key (dashboard: \"Use with AI apps\", or harnessctl ai)")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="home-harness operator API"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "operator authentication required: sign in to the dashboard with the manager's login link, or send the operator token as a Bearer header",
			})
			return
		}
		need := requiredRole(pattern, r)
		if p.role.Rank() < need.Rank() {
			s.auditRejection("operator.forbidden", "", p.actor, "role "+string(p.role)+" needs "+string(need)+" for "+clip(pattern), nil)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "your role (" + string(p.role) + ") cannot do this: it needs " + string(need) + ". Ask an admin, or use a token with that role.",
			})
			return
		}
		ctx := withActor(r.Context(), p.actor)
		ctx = context.WithValue(ctx, principalKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principal is who an authenticated request is.
type principal struct {
	user  string // "" for the legacy operator token
	role  domain.Role
	actor string // what the audit log records
}

type principalKey struct{}

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey{}).(principal)
	return p, ok
}

// legacyEpoch marks a session for the operator token (no user record).
const legacyEpoch = "operator"

// authenticate resolves a bearer value to a principal: the operator token
// or its old (non-expiring) session are admin; a signed session or a user
// token belongs to that user, with the role the user has now.
func (s *Server) authenticate(got string, now time.Time) (principal, bool) {
	if got == "" {
		return principal{}, false
	}
	token := s.operatorToken()
	if subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
		return principal{role: domain.RoleAdmin, actor: actorOperatorToken}, true
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(dashboardSession(token))) == 1 {
		return principal{role: domain.RoleAdmin, actor: actorDashboardSession}, true
	}
	if name, epoch, ok := parseSession(token, got, now); ok {
		if name == "" {
			if epoch == legacyEpoch {
				return principal{role: domain.RoleAdmin, actor: actorDashboardSession}, true
			}
			return principal{}, false
		}
		if u, ok := s.currentUser(name, now); ok && subtle.ConstantTimeCompare([]byte(u.Epoch), []byte(epoch)) == 1 {
			return principal{user: u.Name, role: u.Role, actor: u.Name}, true
		}
		return principal{}, false
	}
	if u, ok := s.userByToken(got, now); ok {
		return principal{user: u.Name, role: u.Role, actor: u.Name}, true
	}
	return principal{}, false
}

// authSource is what failed attempts are counted against.
func authSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// apiLogin exchanges the operator token (or a user's token) for a dashboard
// session. The session goes in the response body for the dashboard to keep
// in localStorage — deliberately never in a Set-Cookie (see the top of this
// file for why a cookie would leak to other localhost ports). Sessions
// expire after userSessionTTL; the dashboard then asks to sign in again.
func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token := s.operatorToken()
	if token == "" {
		writeJSON(w, http.StatusOK, map[string]string{"session": ""}) // auth disabled
		return
	}
	now := time.Now()
	exp := now.Add(userSessionTTL)
	if req.Token != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(token)) == 1 {
		s.audit(domain.AuditSecurity, "operator.login", "", actorDashboardSession, map[string]any{"remote": r.RemoteAddr})
		writeJSON(w, http.StatusOK, map[string]any{"session": signSession(token, "", legacyEpoch, exp), "role": domain.RoleAdmin, "expires": exp.UTC()})
		return
	}
	if u, ok := s.userByToken(req.Token, now); ok {
		s.audit(domain.AuditSecurity, "operator.login", "", u.Name, map[string]any{"remote": r.RemoteAddr})
		writeJSON(w, http.StatusOK, map[string]any{"session": signSession(token, u.Name, u.Epoch, exp), "user": u.Name, "role": u.Role, "expires": exp.UTC()})
		return
	}
	if s.authFail.tooMany(authSource(r), now) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed sign-in attempts from this source: wait a minute and try again"})
		return
	}
	s.auditRejection("operator.login-failed", "", r.RemoteAddr, "invalid login token", nil)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "that login link is not valid for this manager"})
}

// apiWhoAmI tells a client who it is signed in as and what that may do.
func (s *Server) apiWhoAmI(w http.ResponseWriter, r *http.Request) {
	if p, ok := principalFrom(r.Context()); ok {
		name := p.user
		if name == "" {
			name = p.actor
		}
		writeJSON(w, http.StatusOK, map[string]string{"user": name, "role": string(p.role)})
		return
	}
	role := domain.RoleAdmin // authentication disabled (tests, embedding)
	if remoteCallFrom(r.Context()) != nil {
		role = domain.RoleViewer
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": actorFrom(r.Context()), "role": string(role)})
}

const serverProofContext = "harness-server-proof-v1:"

// serverProof is what GET /server-proof answers for nonce.
func serverProof(token, nonce string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(serverProofContext + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// apiServerProof lets a client that holds the token check that it is
// talking to this manager before it sends the token anywhere. On Android
// any app can listen on 127.0.0.1:7421 while the manager is down (stopped,
// restarting); the Android app would otherwise load that app's page with
// the login link, and that page would also read the dashboard session
// from localStorage (same origin). It answers HMAC(token, context+nonce)
// for a client-chosen nonce: only the real manager can, and the answers
// reveal nothing that works as a credential (the context differs from
// the session's).
func (s *Server) apiServerProof(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if len(nonce) < 32 || len(nonce) > 128 || strings.Trim(nonce, "0123456789abcdef") != "" {
		http.Error(w, "nonce must be 32-128 lowercase hex characters", http.StatusBadRequest)
		return
	}
	if s.operatorToken() == "" {
		http.Error(w, "this manager has no operator token", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"proof": serverProof(s.operatorToken(), nonce)})
}

// LoginURL is the dashboard link that signs a browser in: the token rides
// in the fragment, which browsers never send to a server, and the
// dashboard exchanges and then scrubs it from the address bar and history.
func LoginURL(apiAddr, token string) string {
	host := apiAddr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return "http://" + host + "/#login=" + token
}
