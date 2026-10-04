package manager

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/remoteaccess"
)

// Remote access: the owner's dashboard from anywhere, through the relay,
// without letting the relay (or anyone else) act as the owner. The wire
// protocol, why it is not end-to-end TLS, and exactly what a malicious
// relay can and cannot do are in internal/remoteaccess; this file is the
// manager's half:
//
//   - Off by default. The operator turns it on locally (harnessctl remote
//     on, or the dashboard's "Remote access" card), which never works
//     remotely: the settings routes are local-only.
//   - Its own credential, the remote key (120 bits, generated here, never
//     the operator token). The relay sees only a login proof bound to a
//     single-use server nonce, and calls sealed with session keys.
//   - Served only on the relay listener (/remote/, cmd/manager), never on
//     the LAN port or the loopback API. Every agent that can reach the
//     relay session can reach these routes too, so everything is checked
//     here: the relay is not trusted to authenticate anything.
//   - A sealed call is opened, then handed to the very same operator API
//     handler as a local request (NewHTTPHandler: guard, auth, routes),
//     marked as a remote call with actor "remote". requireOperator sends
//     it through remoteGate, which looks up the route it matches on that
//     same mux and refuses it unless the remote mode allows that route.
//     Routes nobody classified (new ones, e.g. standby/promotion) are
//     refused: allow-listed, never deny-listed.
//   - Rate-limited (logins globally, calls per session), and audited: every
//     call in the "remote" audit log, sign-ins and setting changes in the
//     security log, and actions the existing handlers audit with actor
//     "remote".
//
// Modes:
//   - read-only: only routes that show things;
//   - standard (the default): also run work (tasks, jobs, plans, commands
//     other than updates, availability, files);
//   - full: also the sensitive actions: revoking, policy, adding devices
//     (join window, approvals, invitations), node labels, updates.
//
// Never remote, in any mode: the remote settings themselves, the
// sign-in/proof routes, the dashboard shells, and routes whose answer is a
// long-lived secret (GET /join-info and /join-script hold the pairing and
// relay tokens, /ai-info the AI key): even a page swapped by a malicious
// relay must not be able to walk away with something that outlives a key
// rotation.

const actorRemote = "remote"

// RemoteMode is how much remote access may do.
type RemoteMode string

const (
	RemoteReadOnly RemoteMode = "read-only"
	RemoteStandard RemoteMode = "standard"
	RemoteFull     RemoteMode = "full"
)

func (m RemoteMode) valid() bool {
	return m == RemoteReadOnly || m == RemoteStandard || m == RemoteFull
}

const (
	remoteMaxSessions = 8
	remoteIdle        = 20 * time.Minute
	remoteMaxAge      = 8 * time.Hour
	remoteNonceTTL    = 2 * time.Minute
	remoteUsedMax     = 4096
	// Sign-in: at most remoteMaxFailures wrong keys per window, then
	// sign-in pauses until the window passes (the key is 120 bits, so this
	// mostly keeps the audit log and CPU calm); hello and login together
	// are limited to remotePreRate per second (30 a minute).
	remoteMaxFailures   = 10
	remoteFailureWindow = 10 * time.Minute
	remotePreRate       = 0.5
	remotePreBurst      = 30
	// Calls: per session, and a global ceiling for calls refused before
	// they could be verified (unknown session, tampered, replayed).
	remoteCallRate     = 10.0
	remoteCallBurst    = 60
	remoteMaxInflight  = 16
	remoteInvalidRate  = 2.0
	remoteInvalidBurst = 30
	// remoteBodyBudget bounds the request bodies held in memory at once
	// across all remote calls (each up to remoteaccess.MaxSealedRequest).
	remoteBodyBudget = 32 << 20
)

// remoteSettings is what survives a restart (Config.RemoteAccessFile).
type remoteSettings struct {
	Enabled bool       `json:"enabled"`
	Mode    RemoteMode `json:"mode"`
	Key     string     `json:"key,omitempty"`
	URL     string     `json:"url,omitempty"`
}

type remoteSession struct {
	id       string
	c2s, s2c cipher.AEAD
	created  time.Time
	ctx      context.Context // canceled when the session ends: stops its streams
	cancel   context.CancelFunc

	// guarded by remoteAccess.mu
	lastUsed time.Time
	window   remoteaccess.ReplayWindow
	bucket   tokenBucket
	inflight int
}

type remoteAccess struct {
	file     string
	nonceKey []byte

	mu        sync.Mutex
	settings  remoteSettings
	secret    *remoteaccess.Secret
	used      map[string]time.Time // spent server nonces, until they would have expired anyway
	sessions  map[string]*remoteSession
	pre       tokenBucket
	invalid   tokenBucket
	failures  []time.Time
	bodyBytes int64

	apiOnce sync.Once
	api     http.Handler
}

func newRemoteAccess(file string) *remoteAccess {
	a := &remoteAccess{file: file, nonceKey: make([]byte, 32), used: make(map[string]time.Time),
		sessions: make(map[string]*remoteSession), settings: remoteSettings{Mode: RemoteStandard}}
	if _, err := rand.Read(a.nonceKey); err != nil {
		panic(err)
	}
	if file == "" {
		return a
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return a
	}
	var st remoteSettings
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err == nil && !st.Mode.valid() {
		err = fmt.Errorf("unknown mode %q", st.Mode)
	}
	var secret *remoteaccess.Secret
	if err == nil && st.Key != "" {
		secret, err = remoteaccess.NewSecret(st.Key)
	}
	if err != nil {
		log.Printf("manager: remote access is OFF: %s: %v", file, err)
		return a
	}
	a.settings, a.secret = st, secret
	if st.Enabled && secret != nil {
		log.Printf("manager: remote access is ON (%s) at %s", st.Mode, st.URL)
	}
	return a
}

// save writes the settings atomically, owner-only. Called with mu held.
func (a *remoteAccess) save() error {
	if a.file == "" {
		return nil
	}
	data, err := json.MarshalIndent(a.settings, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.file)
}

// on reports whether remote access is on (and has a key), with its mode.
func (a *remoteAccess) on() (bool, RemoteMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings.Enabled && a.secret != nil, a.settings.Mode
}

// dropSessionsLocked ends every session, canceling its open calls.
func (a *remoteAccess) dropSessionsLocked() {
	for id, sess := range a.sessions {
		sess.cancel()
		delete(a.sessions, id)
	}
}

// sessionLocked returns a live session, forgetting it if it has expired.
func (a *remoteAccess) sessionLocked(id string, now time.Time) *remoteSession {
	sess := a.sessions[id]
	if sess == nil {
		return nil
	}
	if now.Sub(sess.created) > remoteMaxAge || (sess.inflight == 0 && now.Sub(sess.lastUsed) > remoteIdle) {
		sess.cancel()
		delete(a.sessions, id)
		return nil
	}
	return sess
}

// tokenBucket is a plain token-bucket limiter; the caller holds the lock.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) take(now time.Time, rate, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Server nonces are stateless until spent: issue time, randomness and a
// MAC under a per-process key, so hello stores nothing (it can't be used
// to fill memory) and a manager restart voids every outstanding one.
func (a *remoteAccess) newNonce(now time.Time) string {
	b := make([]byte, 24, 40)
	binary.BigEndian.PutUint64(b, uint64(now.Unix()))
	rand.Read(b[8:])
	m := hmac.New(sha256.New, a.nonceKey)
	m.Write(b)
	return hex.EncodeToString(m.Sum(b)[:40])
}

// spendNonceLocked accepts a nonce this manager issued, recently, once.
func (a *remoteAccess) spendNonceLocked(sn string, now time.Time) bool {
	raw, err := hex.DecodeString(sn)
	if err != nil || len(raw) != 40 {
		return false
	}
	m := hmac.New(sha256.New, a.nonceKey)
	m.Write(raw[:24])
	if !hmac.Equal(m.Sum(nil)[:16], raw[24:]) {
		return false
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(raw)), 0)
	if now.Sub(issued) > remoteNonceTTL || issued.After(now.Add(5*time.Second)) {
		return false
	}
	for k, exp := range a.used {
		if now.After(exp) {
			delete(a.used, k)
		}
	}
	if _, spent := a.used[sn]; spent || len(a.used) >= remoteUsedMax {
		return false
	}
	a.used[sn] = issued.Add(remoteNonceTTL + 5*time.Second)
	return true
}

// signInPausedLocked reports how long sign-in stays paused after too many
// wrong keys (0: not paused).
func (a *remoteAccess) signInPausedLocked(now time.Time) time.Duration {
	kept := a.failures[:0]
	for _, t := range a.failures {
		if now.Sub(t) < remoteFailureWindow {
			kept = append(kept, t)
		}
	}
	a.failures = kept
	if len(kept) < remoteMaxFailures {
		return 0
	}
	return remoteFailureWindow - now.Sub(kept[0])
}

// ---- the relay-facing routes (mounted at /remote/ on the relay listener)

//go:embed remote.js
var remoteJS string

// remotePage is the dashboard with the remote client (remote.js) inlined
// before its own script, and a CSP allowing exactly those two scripts (by
// hash), same-origin fetches, and no framing. The CSP guards against
// agent-supplied text on an honest relay; a malicious relay could strip
// it, which is part of "it controls the page" (internal/remoteaccess).
var remotePage = sync.OnceValues(func() ([]byte, string) {
	page := bytes.Replace(dashboardHTML, []byte("<script>"), []byte("<script>\n"+remoteJS+"</script>\n<script>"), 1)
	var hashes []string
	for _, m := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllSubmatch(page, -1) {
		sum := sha256.Sum256(m[1])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	csp := "default-src 'none'; script-src " + strings.Join(hashes, " ") +
		"; style-src 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	return page, csp
})

// RemoteHandler serves the remote dashboard's routes under /remote/. Mount
// it on the relay listener only (cmd/manager).
func (s *Server) RemoteHandler() http.HandlerFunc {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /remote/{$}", s.remotePageHandler)
	mux.HandleFunc("POST /remote/hello", s.remoteHello)
	mux.HandleFunc("POST /remote/login", s.remoteLogin)
	mux.HandleFunc("POST /remote/call", s.remoteCall)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if on, _ := s.remote.on(); !on {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, "Remote access to this manager is off. Turn it on at home: harnessctl remote on, or the dashboard's \"Remote access\" card.\n")
				return
			}
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "remote access to this manager is off", "signedOut": true})
			return
		}
		mux.ServeHTTP(w, r)
	}
}

func (s *Server) remotePageHandler(w http.ResponseWriter, r *http.Request) {
	page, csp := remotePage()
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.Write(page)
}

func (s *Server) remoteHello(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	a := s.remote
	a.mu.Lock()
	ok := a.pre.take(now, remotePreRate, remotePreBurst)
	a.mu.Unlock()
	if !ok {
		remoteTooMany(w, time.Second)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"nonce": a.newNonce(now)})
}

func remoteTooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(max(wait, time.Second).Seconds())))
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts: wait a little and try again"})
}

// via is the browser address as the relay reports it: a hint for the
// audit log that the relay could set to anything.
func remoteVia(r *http.Request) string {
	if v := r.Header.Get(remoteaccess.HeaderClient); v != "" {
		return clip(v) + " (as reported by the relay)"
	}
	return "unknown"
}

var hexNonce = regexp.MustCompile(`^[0-9a-f]{32,64}$`)

func (s *Server) remoteLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nonce       string `json:"nonce"`
		ClientNonce string `json:"clientNonce"`
		Proof       string `json:"proof"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || !hexNonce.MatchString(req.ClientNonce) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed sign-in"})
		return
	}
	now := time.Now()
	a := s.remote
	a.mu.Lock()
	if wait := a.signInPausedLocked(now); wait > 0 {
		a.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "remote sign-in is paused after too many wrong keys; try again in a few minutes"})
		return
	}
	if !a.pre.take(now, remotePreRate, remotePreBurst) {
		a.mu.Unlock()
		remoteTooMany(w, time.Second)
		return
	}
	if !a.spendNonceLocked(req.Nonce, now) {
		a.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the sign-in challenge expired or was already used; try again"})
		return
	}
	proof, err := hex.DecodeString(req.Proof)
	if err != nil || !remoteaccess.Equal(proof, a.secret.ClientProof(req.Nonce, req.ClientNonce)) {
		a.failures = append(a.failures, now)
		a.mu.Unlock()
		s.auditRejection("remote.login-failed", "", remoteVia(r), "wrong remote key", nil)
		// 403, not 401: the page forgets a remembered key only on this.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "that remote key is not right for this manager"})
		return
	}
	c2s, s2c, err := a.secret.SessionKeys(req.Nonce, req.ClientNonce)
	if err != nil {
		a.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := randomToken()
	if err != nil {
		a.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess := &remoteSession{id: id, c2s: c2s, s2c: s2c, created: now, lastUsed: now, ctx: ctx, cancel: cancel}
	for len(a.sessions) >= remoteMaxSessions { // the least recently used goes
		var oldest *remoteSession
		for _, o := range a.sessions {
			if oldest == nil || o.lastUsed.Before(oldest.lastUsed) {
				oldest = o
			}
		}
		oldest.cancel()
		delete(a.sessions, oldest.id)
	}
	a.sessions[id] = sess
	mode := a.settings.Mode
	serverProof := hex.EncodeToString(a.secret.ServerProof(req.Nonce, req.ClientNonce))
	a.mu.Unlock()
	s.audit(domain.AuditSecurity, "remote.login", "", actorRemote, map[string]any{"session": id[:8], "mode": string(mode), "via": remoteVia(r)})
	writeJSON(w, http.StatusOK, map[string]any{"session": id, "serverProof": serverProof, "mode": mode, "idleSeconds": int(remoteIdle.Seconds())})
}

// remoteCall opens one sealed call, runs it through the operator API as
// a remote call, and seals the answer. Refusals before the call is
// verified are plain (a relay could fake those anyway: the client treats
// them only as errors); from then on every answer is sealed.
func (s *Server) remoteCall(w http.ResponseWriter, r *http.Request) {
	a := s.remote
	sid := r.Header.Get(remoteaccess.HeaderSession)
	seq, seqErr := strconv.ParseUint(r.Header.Get(remoteaccess.HeaderSeq), 10, 64)
	if r.ContentLength > remoteaccess.MaxSealedRequest {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("remote requests are limited to %d MiB", remoteaccess.MaxBody>>20)})
		return
	}
	reserve := int64(remoteaccess.MaxSealedRequest)
	if r.ContentLength >= 0 {
		reserve = r.ContentLength
	}
	now := time.Now()
	a.mu.Lock()
	sess := a.sessionLocked(sid, now)
	if sess == nil || seqErr != nil {
		ok := a.invalid.take(now, remoteInvalidRate, remoteInvalidBurst)
		a.mu.Unlock()
		if !ok {
			remoteTooMany(w, time.Second)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "signed out: sign in again", "signedOut": true})
		return
	}
	if a.bodyBytes+reserve > remoteBodyBudget {
		a.mu.Unlock()
		remoteTooMany(w, time.Second)
		return
	}
	a.bodyBytes += reserve
	a.mu.Unlock()
	sealed, err := io.ReadAll(io.LimitReader(r.Body, remoteaccess.MaxSealedRequest+1))
	a.mu.Lock()
	a.bodyBytes -= reserve
	a.mu.Unlock()
	if err != nil || len(sealed) > remoteaccess.MaxSealedRequest {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request could not be read"})
		return
	}
	call, err := remoteaccess.OpenRequest(sess.c2s, sid, seq, sealed)
	if err != nil {
		s.remoteRefuseUnverified(w, r, "could not be verified", http.StatusForbidden, "this request could not be verified (it was changed on the way, or belongs to another session)")
		return
	}
	a.mu.Lock()
	switch {
	case !sess.window.Accept(seq):
		a.mu.Unlock()
		s.remoteRefuseUnverified(w, r, "replayed", http.StatusConflict, "this request was already used")
		return
	case !sess.bucket.take(now, remoteCallRate, remoteCallBurst), sess.inflight >= remoteMaxInflight:
		a.mu.Unlock()
		remoteTooMany(w, time.Second)
		return
	}
	sess.inflight++
	sess.lastUsed = now
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		sess.inflight--
		sess.lastUsed = time.Now()
		a.mu.Unlock()
	}()

	sw := newSealedWriter(w, sess.s2c, sid, seq)
	defer sw.finish()
	rc := &remoteCall{session: sid, body: call.Body}
	if call.Method == http.MethodPost && call.Path == remoteaccess.LogoutPath {
		a.mu.Lock()
		if a.sessions[sid] == sess {
			delete(a.sessions, sid)
		}
		a.mu.Unlock()
		s.audit(domain.AuditSecurity, "remote.logout", "", actorRemote, map[string]any{"session": sid[:8]})
		writeJSON(sw, http.StatusOK, map[string]bool{"ok": true})
		sess.cancel() // after the answer is written: it ends this session's streams
		return
	}
	// The call ends with the request or the session, whichever is first.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(sess.ctx, cancel)
	defer stop()
	inner, err := remoteInnerRequest(withActor(withRemoteCall(ctx, rc), actorRemote), call, sid)
	if err != nil {
		s.auditRemoteRequest(rc, call.Method, call.Path, "", err.Error())
		writeJSON(sw, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.remote.apiOnce.Do(func() { s.remote.api = s.NewHTTPHandler() })
	s.remote.api.ServeHTTP(sw, inner)
}

// remoteRefuseUnverified answers a call that failed verification. It goes
// to the noise log (rate-limited): whoever sent it is not proven to be
// the owner, so it must not compete with the owner's own records.
func (s *Server) remoteRefuseUnverified(w http.ResponseWriter, r *http.Request, reason string, status int, msg string) {
	a := s.remote
	a.mu.Lock()
	ok := a.invalid.take(time.Now(), remoteInvalidRate, remoteInvalidBurst)
	a.mu.Unlock()
	if !ok {
		remoteTooMany(w, time.Second)
		return
	}
	s.auditRejection("remote.call-refused", "", remoteVia(r), reason, nil)
	writeJSON(w, status, map[string]string{"error": msg})
}

// remoteInnerRequest builds the operator API request a sealed call asks
// for, addressed exactly like a local one (so guardOperatorAPI passes it)
// and refusing anything but a plain absolute path: the route remoteGate
// classifies must be the route the mux then runs.
func remoteInnerRequest(ctx context.Context, call remoteaccess.Request, sid string) (*http.Request, error) {
	switch call.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return nil, fmt.Errorf("method %q is not available remotely", call.Method)
	}
	u, err := url.ParseRequestURI(call.Path)
	if err != nil || !strings.HasPrefix(call.Path, "/") || strings.HasPrefix(call.Path, "//") ||
		strings.ContainsAny(call.Path, "#\\") || u.Scheme != "" || u.Host != "" || (u.Path != "/" && path.Clean(u.Path) != u.Path) {
		return nil, errors.New("not a plain API path")
	}
	inner, err := http.NewRequestWithContext(ctx, call.Method, "http://127.0.0.1"+call.Path, bytes.NewReader(call.Body))
	if err != nil {
		return nil, errors.New("not a plain API path")
	}
	inner.Host = "127.0.0.1"
	inner.RemoteAddr = "remote:" + sid[:8]
	if call.ContentType != "" {
		inner.Header.Set("Content-Type", call.ContentType)
	}
	return inner, nil
}

type remoteCallKey struct{}

// remoteCall marks an operator API request as a verified remote call. It
// lives only in the request context, which nothing on the wire can set.
type remoteCall struct {
	session string
	body    []byte
}

func withRemoteCall(ctx context.Context, c *remoteCall) context.Context {
	return context.WithValue(ctx, remoteCallKey{}, c)
}

func remoteCallFrom(ctx context.Context) *remoteCall {
	c, _ := ctx.Value(remoteCallKey{}).(*remoteCall)
	return c
}

// ---- what each mode may do

type remoteClass int

const (
	remoteNever     remoteClass = iota // not remotely, in any mode
	remoteView                         // shows things: every mode
	remoteRun                          // runs work, no admin changes: standard and full
	remoteSensitive                    // admin changes: full only
)

// remoteRoutes classifies operator API routes by the exact pattern
// NewHTTPHandler registers. A route missing here is refused remotely: add
// new routes deliberately (and keep secrets out of view/run).
var remoteRoutes = map[string]remoteClass{
	"GET /nodes":                       remoteView,
	"GET /nodes/{id}":                  remoteView,
	"GET /nodes/{id}/resources":        remoteView,
	"GET /nodes/{id}/capabilities":     remoteView,
	"GET /resources/total":             remoteView,
	"GET /events":                      remoteView,
	"GET /revocations":                 remoteView,
	"GET /agent-binaries":              remoteView,
	"GET /agent-binary/hash":           remoteView,
	"GET /join-requests":               remoteView,
	"GET /join-window":                 remoteView,
	"GET /audit":                       remoteView,
	"GET /workloads":                   remoteView,
	"GET /workloads/{id}":              remoteView,
	"GET /artifacts":                   remoteView,
	"GET /artifacts/{sha}":             remoteView,
	"GET /catalog":                     remoteView,
	"GET /models":                      remoteView,
	"GET /ai-devices":                  remoteView,
	"GET /llm/split":                   remoteView,
	"GET /policy":                      remoteView,
	"GET /jobs":                        remoteView,
	"GET /jobs/{id}":                   remoteView,
	"GET /plans":                       remoteView,
	"GET /plans/{id}":                  remoteView,
	"POST /nodes/{id}/commands":        remoteRun, // updates excepted: remoteCommandAllowed
	"POST /workloads":                  remoteRun,
	"POST /workloads/{id}/cancel":      remoteRun,
	"POST /artifacts":                  remoteRun,
	"DELETE /artifacts/{sha}":          remoteRun,
	"PUT /nodes/{id}/availability":     remoteRun,
	"POST /llm/split":                  remoteRun,
	"DELETE /llm/split":                remoteRun,
	"POST /jobs":                       remoteRun,
	"POST /jobs/{id}/cancel":           remoteRun,
	"POST /plans":                      remoteRun,
	"POST /plans/{id}/approve":         remoteRun,
	"POST /plans/{id}/reject":          remoteRun,
	"POST /nodes/{id}/update":          remoteSensitive,
	"POST /nodes/{id}/revoke":          remoteSensitive,
	"DELETE /revocations/{id}":         remoteSensitive,
	"PUT /policy":                      remoteSensitive,
	"POST /join-window":                remoteSensitive,
	"DELETE /join-window":              remoteSensitive,
	"POST /join-requests/{id}/approve": remoteSensitive,
	"POST /join-requests/{id}/reject":  remoteSensitive,
	"POST /enrollments":                remoteSensitive,
	// Labels steer the policy's per-type node labels, so they count as a
	// policy change; the alias rides on the same route.
	"PUT /nodes/{id}/meta": remoteSensitive,
	// Listed only for the record; absent would mean the same.
	"GET /join-info":              remoteNever, // pairing and relay tokens
	"GET /join-script":            remoteNever, // the pairing token
	"GET /ai-info":                remoteNever, // the AI key
	"GET /enrollments/{token}/qr": remoteNever,
	"GET /{$}":                    remoteNever,
	"GET /live":                   remoteNever,
	"POST /login":                 remoteNever,
	"GET /server-proof":           remoteNever,
	"GET /v1/models":              remoteNever,
	"POST /v1/chat/completions":   remoteNever,
	"GET /remote-access":          remoteNever,
	"PUT /remote-access":          remoteNever,
	"POST /remote-access/rotate":  remoteNever,
}

// remoteCommandAllowed: commands other than SELF_UPDATE are diagnostics;
// an update is sensitive however it is asked for.
func remoteCommandAllowed(body []byte) bool {
	var req struct {
		Name domain.CommandName `json:"name"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	switch req.Name {
	case domain.CommandPing, domain.CommandEcho, domain.CommandGetSystemInfo, domain.CommandGetAgentStatus, domain.CommandRequestResourceRefresh:
		return true
	}
	return false
}

// remoteDecision says whether mode allows the route pattern matched, and
// if not, why in words for the owner.
func remoteDecision(pattern string, mode RemoteMode, body []byte) (bool, string) {
	class, known := remoteRoutes[pattern]
	if pattern == "POST /nodes/{id}/commands" && !remoteCommandAllowed(body) {
		class = remoteSensitive
	}
	switch {
	case !known || class == remoteNever:
		return false, "That isn't available remotely: use the manager's own dashboard, at home."
	case class == remoteSensitive && mode != RemoteFull:
		return false, "That is an administrative change (revoking, policy, adding devices, labels or updates). Remote access allows those only with full control: turn it on at home with \"harnessctl remote on full\", or do it on the manager's own dashboard."
	case class == remoteRun && mode == RemoteReadOnly:
		return false, "Remote access is view-only, and this would change something. To allow running tasks remotely, use \"harnessctl remote on\" at home."
	}
	return true, ""
}

// remoteGate wraps the operator API's routes (requireOperator puts it
// around the mux): local requests pass untouched; a remote call is
// classified by the route the mux will actually run, then refused or
// passed on (its actor is already "remote").
func (s *Server) remoteGate(next http.Handler) http.Handler {
	mux, _ := next.(*http.ServeMux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := remoteCallFrom(r.Context())
		if call == nil {
			next.ServeHTTP(w, r)
			return
		}
		pattern := ""
		if mux != nil {
			_, pattern = mux.Handler(r)
		}
		on, mode := s.remote.on()
		allowed, why := remoteDecision(pattern, mode, call.body)
		if !on {
			allowed, why = false, "Remote access was just turned off."
		}
		s.auditRemoteRequest(call, r.Method, r.URL.Path, pattern, why)
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": why})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auditRemoteRequest records one remote call in the remote log; refused
// is empty for a call that was let through.
func (s *Server) auditRemoteRequest(call *remoteCall, method, urlPath, pattern, refused string) {
	detail := map[string]any{"session": call.session[:8], "method": method, "path": clip(urlPath)}
	if pattern != "" {
		detail["route"] = pattern
	}
	if refused != "" {
		detail["refused"] = refused
	}
	s.audit(domain.AuditRemote, "remote.request", "", actorRemote, detail)
}

// sealedWriter is the http.ResponseWriter a remote call's handler writes
// to: status, content type and body go out as sealed frames. Flush seals
// what is buffered and pushes it out, so the event stream stays live.
type sealedWriter struct {
	out      http.ResponseWriter
	frames   *remoteaccess.FrameWriter
	header   http.Header
	status   int
	headSent bool
	buf      []byte
	err      error
}

func newSealedWriter(w http.ResponseWriter, s2c cipher.AEAD, sid string, seq uint64) *sealedWriter {
	return &sealedWriter{out: w, frames: remoteaccess.NewFrameWriter(w, s2c, sid, seq), header: http.Header{}}
}

func (s *sealedWriter) Header() http.Header { return s.header }

func (s *sealedWriter) WriteHeader(code int) {
	if s.status == 0 && code >= 200 {
		s.status = code
	}
}

func (s *sealedWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.err != nil {
		return 0, s.err
	}
	s.buf = append(s.buf, p...)
	if len(s.buf) >= 64<<10 {
		s.flushData()
	}
	return len(p), s.err
}

func (s *sealedWriter) flushData() {
	if s.err != nil {
		return
	}
	if !s.headSent {
		s.headSent = true
		if s.status == 0 {
			s.status = http.StatusOK
		}
		h := s.out.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set(remoteaccess.HeaderSealed, "1")
		h.Set("X-Content-Type-Options", "nosniff")
		s.out.WriteHeader(http.StatusOK)
		s.err = s.frames.Head(remoteaccess.ResponseHead{Status: s.status, Type: s.header.Get("Content-Type")})
	}
	if len(s.buf) > 0 && s.err == nil {
		s.err = s.frames.Data(s.buf)
		s.buf = s.buf[:0]
	}
}

func (s *sealedWriter) Flush() {
	s.flushData()
	http.NewResponseController(s.out).Flush()
}

func (s *sealedWriter) finish() {
	s.flushData()
	if s.err == nil {
		s.err = s.frames.End()
	}
	http.NewResponseController(s.out).Flush()
}

// ---- the operator's controls (local only: never remote, see remoteRoutes)

// RemoteDashboardPublisher is the optional half of an EnrollmentPublisher
// that asks the relay's public gateway for this manager's remote dashboard
// address.
type RemoteDashboardPublisher interface {
	PublishRemoteDashboard(ctx context.Context, session, fingerprint string) (path string, err error)
}

// PublishRemoteDashboard asks the relay for this manager's address
// (/r/<token>/): its relay session and TLS fingerprint sealed under the
// relay's alias key, so it stays valid across relay restarts.
func (p *HTTPEnrollmentPublisher) PublishRemoteDashboard(ctx context.Context, session, fingerprint string) (string, error) {
	body, err := json.Marshal(map[string]string{"session": session, "fingerprint": fingerprint})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/_harness/remote-dashboards", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.PublishToken)
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("relay returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var result struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !strings.HasPrefix(result.Path, "/r/") || !strings.HasSuffix(result.Path, "/") {
		return "", fmt.Errorf("relay returned an unexpected dashboard path")
	}
	return result.Path, nil
}

// remoteUnavailable says why remote access can't be turned on ("" if it
// can).
func (s *Server) remoteUnavailable() string {
	_, publishes := s.cfg.EnrollmentPublisher.(RemoteDashboardPublisher)
	switch {
	case s.cfg.Fingerprint == "":
		return "this manager runs -insecure (no TLS), so remote access is off"
	case s.cfg.RelayToken == "" || s.cfg.RelayPublicURL == "" || !publishes:
		return "remote access goes through your relay's public HTTPS address: start the manager with -relay-addr, -relay-token, -relay-public-url and -relay-enrollment-token"
	}
	return ""
}

// RemoteAccessView is GET /remote-access.
type RemoteAccessView struct {
	Enabled bool       `json:"enabled"`
	Mode    RemoteMode `json:"mode"`
	URL     string     `json:"url,omitempty"`
	Key     string     `json:"key,omitempty"`
	// Sessions is how many remote browsers are signed in now.
	Sessions    int    `json:"sessions"`
	Available   bool   `json:"available"`
	Unavailable string `json:"unavailable,omitempty"`
}

func (s *Server) remoteView() RemoteAccessView {
	why := s.remoteUnavailable()
	a := s.remote
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id := range a.sessions {
		a.sessionLocked(id, now)
	}
	return RemoteAccessView{Enabled: a.settings.Enabled && a.secret != nil, Mode: a.settings.Mode, URL: a.settings.URL,
		Key: a.settings.Key, Sessions: len(a.sessions), Available: why == "", Unavailable: why}
}

func (s *Server) apiGetRemoteAccess(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.remoteView())
}

// apiPutRemoteAccess turns remote access on or off and sets its mode:
// {"enabled":true,"mode":"read-only|standard|full"}. Turning it on the
// first time creates the key and asks the relay for the address.
func (s *Server) apiPutRemoteAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool       `json:"enabled"`
		Mode    RemoteMode `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Mode != "" && !req.Mode.valid() {
		http.Error(w, "mode must be read-only, standard or full", http.StatusBadRequest)
		return
	}
	if err := s.SetRemoteAccess(r.Context(), req.Enabled, req.Mode, actorFrom(r.Context())); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, s.remoteView())
}

// SetRemoteAccess turns remote access on or off; an empty mode keeps the
// current one.
func (s *Server) SetRemoteAccess(ctx context.Context, enabled bool, mode RemoteMode, actor string) error {
	a := s.remote
	if enabled {
		if why := s.remoteUnavailable(); why != "" {
			return errors.New(why)
		}
	}
	a.mu.Lock()
	url := a.settings.URL
	a.mu.Unlock()
	base := strings.TrimRight(s.cfg.RelayPublicURL, "/")
	if enabled && (url == "" || !strings.HasPrefix(url, base+"/r/")) {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		p, err := s.cfg.EnrollmentPublisher.(RemoteDashboardPublisher).PublishRemoteDashboard(pctx, s.cfg.RelayToken, s.cfg.Fingerprint)
		cancel()
		if err != nil {
			return fmt.Errorf("could not get the remote address from the relay: %w", err)
		}
		url = base + p
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.settings
	next := old
	next.Enabled = enabled
	if mode != "" {
		next.Mode = mode
	}
	if enabled {
		next.URL = url
		if next.Key == "" {
			key, err := remoteaccess.NewKey()
			if err != nil {
				return err
			}
			next.Key = key
		}
	}
	secret := a.secret
	if next.Key != old.Key || secret == nil {
		var err error
		if secret, err = remoteaccess.NewSecret(next.Key); err != nil && enabled {
			return err
		}
	}
	a.settings, a.secret = next, secret
	if err := a.save(); err != nil {
		a.settings = old
		return fmt.Errorf("save remote access settings: %w", err)
	}
	if !enabled {
		a.dropSessionsLocked()
	}
	switch {
	case enabled && !old.Enabled:
		s.audit(domain.AuditSecurity, "remote.enabled", "", actor, map[string]any{"mode": string(next.Mode)})
	case !enabled && old.Enabled:
		s.audit(domain.AuditSecurity, "remote.disabled", "", actor, nil)
	case next.Mode != old.Mode:
		s.audit(domain.AuditSecurity, "remote.mode-changed", "", actor, map[string]any{"from": string(old.Mode), "to": string(next.Mode)})
	}
	return nil
}

// apiRotateRemoteKey replaces the remote key: every remote browser and
// harnessctl -remote is signed out, and the old key opens nothing.
func (s *Server) apiRotateRemoteKey(w http.ResponseWriter, r *http.Request) {
	key, err := remoteaccess.NewKey()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	secret, err := remoteaccess.NewSecret(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a := s.remote
	a.mu.Lock()
	old := a.settings
	a.settings.Key, a.secret = key, secret
	if err := a.save(); err != nil {
		a.settings = old
		a.secret, _ = remoteaccess.NewSecret(old.Key)
		a.mu.Unlock()
		http.Error(w, "save remote access settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.dropSessionsLocked()
	a.mu.Unlock()
	s.audit(domain.AuditSecurity, "remote.key-rotated", "", actorFrom(r.Context()), nil)
	writeJSON(w, http.StatusOK, s.remoteView())
}

// registerRemoteAccessRoutes adds the operator's controls to the API.
func (s *Server) registerRemoteAccessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /remote-access", s.apiGetRemoteAccess)
	mux.HandleFunc("PUT /remote-access", s.apiPutRemoteAccess)
	mux.HandleFunc("POST /remote-access/rotate", s.apiRotateRemoteKey)
}
