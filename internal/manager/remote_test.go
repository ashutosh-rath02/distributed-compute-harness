package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/remoteaccess"
)

type fakeRemotePublisher struct{ calls int }

func (p *fakeRemotePublisher) Publish(context.Context, PublishedEnrollment) (string, error) {
	return "", errors.New("not used here")
}

func (p *fakeRemotePublisher) PublishRemoteDashboard(_ context.Context, session, fingerprint string) (string, error) {
	p.calls++
	if session == "" || len(fingerprint) != 64 {
		return "", errors.New("bad publish")
	}
	return "/r/hr1.test/", nil
}

func newRemoteTestServer(t *testing.T, file string) *Server {
	t.Helper()
	return NewServer(nil, nil, Config{
		OperatorToken: testOperatorToken, PairingToken: "permanent-pairing-secret", AIKey: strings.Repeat("c", 64),
		Fingerprint: strings.Repeat("ab", 32), RelayToken: "relay-session", RelayPublicURL: "https://relay.example",
		EnrollmentPublisher: &fakeRemotePublisher{}, RemoteAccessFile: file,
	})
}

// handlerTransport carries a client's requests straight to a handler,
// in process (no sockets: the integration tests cover the real path).
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func remoteOn(t *testing.T, s *Server, mode RemoteMode) RemoteAccessView {
	t.Helper()
	if err := s.SetRemoteAccess(context.Background(), true, mode, actorOperatorToken); err != nil {
		t.Fatal(err)
	}
	return s.remoteView()
}

func remoteClient(s *Server, key string) *remoteaccess.Client {
	return &remoteaccess.Client{BaseURL: "http://relay/remote/", Key: key, HTTP: &http.Client{Transport: handlerTransport{s.RemoteHandler()}}}
}

// remoteDo makes one remote call and returns its status and body.
func remoteDo(t *testing.T, c *remoteaccess.Client, method, path, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, "http://manager"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the sealed reply: %v", method, path, err)
	}
	return resp.StatusCode, string(b)
}

func TestRemoteDecisionByMode(t *testing.T) {
	ping := []byte(`{"name":"PING"}`)
	update := []byte(`{"name":"SELF_UPDATE","args":{"url":"x"}}`)
	for _, c := range []struct {
		pattern string
		body    []byte
		ro, std bool
		full    bool
	}{
		{"GET /nodes", nil, true, true, true},
		{"GET /events", nil, true, true, true},
		{"GET /audit", nil, true, true, true},
		{"POST /workloads", nil, false, true, true},
		{"POST /jobs", nil, false, true, true},
		{"POST /nodes/{id}/commands", ping, false, true, true},
		{"POST /nodes/{id}/commands", update, false, false, true},
		{"POST /nodes/{id}/commands", []byte(`not json`), false, false, true},
		{"POST /nodes/{id}/update", nil, false, false, true},
		{"POST /nodes/{id}/revoke", nil, false, false, true},
		{"DELETE /revocations/{id}", nil, false, false, true},
		{"PUT /policy", nil, false, false, true},
		{"POST /join-window", nil, false, false, true},
		{"DELETE /join-window", nil, false, false, true},
		{"POST /join-requests/{id}/approve", nil, false, false, true},
		{"POST /enrollments", nil, false, false, true},
		{"PUT /nodes/{id}/meta", nil, false, false, true},
		// Never, even with full control.
		{"GET /join-info", nil, false, false, false},
		{"GET /join-script", nil, false, false, false},
		{"GET /ai-info", nil, false, false, false},
		{"PUT /remote-access", nil, false, false, false},
		{"POST /remote-access/rotate", nil, false, false, false},
		{"POST /login", nil, false, false, false},
		{"POST /v1/chat/completions", nil, false, false, false},
		// Unclassified (a route added later, e.g. standby promotion) and
		// unmatched: refused.
		{"POST /standby/promote", nil, false, false, false},
		{"", nil, false, false, false},
	} {
		for mode, want := range map[RemoteMode]bool{RemoteReadOnly: c.ro, RemoteStandard: c.std, RemoteFull: c.full} {
			if got, why := remoteDecision(c.pattern, mode, c.body); got != want || (!got && why == "") {
				t.Errorf("%q (%s) in %s: allowed=%v (%q), want %v", c.pattern, c.body, mode, got, why, want)
			}
		}
	}
}

// TestRemoteRoutesMatchTheOperatorAPI makes a remote call to every
// classified route (read-only, so nothing but views runs) and checks the
// route the operator API matched, as the remote log records it, is the
// very pattern classified: a typo in remoteRoutes would otherwise just
// refuse a route nobody noticed.
func TestRemoteRoutesMatchTheOperatorAPI(t *testing.T) {
	s := newRemoteTestServer(t, "")
	v := remoteOn(t, s, RemoteReadOnly)
	c := remoteClient(s, v.Key)
	fill := strings.NewReplacer("{id}", "node-x", "{sha}", strings.Repeat("0", 64), "{token}", "tok", "{$}", "")
	for pattern := range remoteRoutes {
		method, path, _ := strings.Cut(pattern, " ")
		if pattern == "GET /events" {
			continue // a stream: covered by the integration tests
		}
		remoteDo(t, c, method, fill.Replace(path), "")
		entries, err := s.AuditEntries(domain.AuditRemote, 1)
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s: no remote log entry (%v)", pattern, err)
		}
		if got := entries[0].Detail["route"]; got != pattern {
			t.Errorf("%s: the operator API matched %v", pattern, got)
		}
	}
}

func TestRemoteModesAreEnforced(t *testing.T) {
	s := newRemoteTestServer(t, "")
	v := remoteOn(t, s, RemoteReadOnly)
	c := remoteClient(s, v.Key)

	if code, body := remoteDo(t, c, "GET", "/nodes", ""); code != http.StatusOK || !strings.HasPrefix(body, "[") {
		t.Fatalf("read-only GET /nodes: %d %s", code, body)
	}
	if code, body := remoteDo(t, c, "POST", "/workloads", `{"capability":"cpu.burn","params":{"seconds":"1"}}`); code != http.StatusForbidden || !strings.Contains(body, "view-only") {
		t.Fatalf("read-only POST /workloads: %d %s", code, body)
	}
	if n := len(s.Workloads.List()); n != 0 {
		t.Fatalf("a refused remote submission created %d workloads", n)
	}

	remoteOn(t, s, RemoteStandard) // a mode change applies to live sessions
	for _, r := range []struct{ method, path, body string }{
		{"PUT", "/policy", `{"allowUnlisted":true}`},
		{"POST", "/nodes/node-x/revoke", ""},
		{"POST", "/nodes/node-x/update", ""},
		{"POST", "/join-window", `{"minutes":5}`},
		{"POST", "/enrollments", `{"platform":"windows"}`},
		{"PUT", "/nodes/node-x/meta", `{"labels":{"trusted":"yes"}}`},
		{"POST", "/nodes/node-x/commands", `{"name":"SELF_UPDATE"}`},
	} {
		if code, body := remoteDo(t, c, r.method, r.path, r.body); code != http.StatusForbidden || !strings.Contains(body, "full control") {
			t.Errorf("standard %s %s: %d %s", r.method, r.path, code, body)
		}
	}
	if code, _ := remoteDo(t, c, "POST", "/nodes/node-x/commands", `{"name":"PING"}`); code == http.StatusForbidden {
		t.Error("standard: a PING was refused")
	}
	if code, _ := remoteDo(t, c, "POST", "/workloads", `{"capability":"cpu.burn","params":{"seconds":"1"}}`); code == http.StatusForbidden {
		t.Error("standard: running a task was refused")
	}

	remoteOn(t, s, RemoteFull)
	if code, body := remoteDo(t, c, "PUT", "/policy", `{"types":{},"allowUnlisted":true}`); code != http.StatusOK {
		t.Fatalf("full PUT /policy: %d %s", code, body)
	}
	for _, r := range []struct{ method, path string }{
		{"GET", "/join-info"}, {"GET", "/join-script?addr=1.2.3.4:7420"}, {"GET", "/ai-info"},
		{"GET", "/remote-access"}, {"PUT", "/remote-access"}, {"POST", "/remote-access/rotate"}, {"GET", "/"},
	} {
		code, body := remoteDo(t, c, r.method, r.path, `{"enabled":true,"mode":"full"}`)
		if code != http.StatusForbidden || strings.Contains(body, "permanent-pairing-secret") || strings.Contains(body, v.Key) {
			t.Errorf("full %s %s: %d %s", r.method, r.path, code, body)
		}
	}

	// Who acted: the handlers' own audit entries say "remote".
	entries, _ := s.AuditEntries(domain.AuditSecurity, 50)
	var policy, login bool
	for _, e := range entries {
		switch e.Kind {
		case "policy.changed":
			policy = e.Actor == actorRemote
		case "remote.login":
			login = e.Actor == actorRemote
		}
	}
	if !policy || !login {
		t.Fatalf("security log actors: policy=%v login=%v: %+v", policy, login, entries)
	}
	// And every call is in the remote log, refusals with their reason.
	remoteLog, _ := s.AuditEntries(domain.AuditRemote, 100)
	refused := 0
	for _, e := range remoteLog {
		if e.Actor != actorRemote {
			t.Fatalf("remote log entry by %q", e.Actor)
		}
		if e.Detail["refused"] != nil {
			refused++
		}
	}
	if len(remoteLog) < 15 || refused < 14 {
		t.Fatalf("remote log: %d entries, %d refused", len(remoteLog), refused)
	}
}

func postJSON(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestRemoteLoginChallengeIsSingleUseAndWrongKeysPause(t *testing.T) {
	s := newRemoteTestServer(t, "")
	v := remoteOn(t, s, RemoteStandard)
	h := s.RemoteHandler()
	secret, _ := remoteaccess.NewSecret(v.Key)
	wrong, _ := remoteaccess.NewSecret(strings.Repeat("A", 24))
	cn := strings.Repeat("ab", 16)
	login := func(sec *remoteaccess.Secret, sn string) (int, map[string]any) {
		return postJSON(t, h, "/remote/login", map[string]string{"nonce": sn, "clientNonce": cn, "proof": hex.EncodeToString(sec.ClientProof(sn, cn))})
	}
	hello := func() string {
		code, out := postJSON(t, h, "/remote/hello", nil)
		if code != http.StatusOK {
			t.Fatalf("hello: %d", code)
		}
		return out["nonce"].(string)
	}

	sn := hello()
	if code, out := login(secret, sn); code != http.StatusOK || out["session"] == "" {
		t.Fatalf("login: %d %v", code, out)
	}
	if code, _ := login(secret, sn); code != http.StatusUnauthorized {
		t.Fatalf("a replayed login: %d, want 401 (the nonce is spent)", code)
	}
	if code, _ := login(secret, strings.Repeat("0", 80)); code != http.StatusUnauthorized {
		t.Fatalf("a nonce this manager never issued: %d", code)
	}
	for i := 0; i < remoteMaxFailures; i++ {
		if code, _ := login(wrong, hello()); code != http.StatusForbidden {
			t.Fatalf("wrong key %d: %d, want 403", i, code)
		}
	}
	if code, _ := login(secret, hello()); code != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong keys the right one: %d, want 429 (paused)", remoteMaxFailures, code)
	}
	noise, _ := s.AuditEntries(domain.AuditNoise, 50)
	if len(noise) == 0 || noise[0].Kind != "remote.login-failed" || noise[0].Actor != actorAnonymous {
		t.Fatalf("failed sign-ins are not in the noise log: %+v", noise)
	}
}

func TestRemoteCallsAreRateLimited(t *testing.T) {
	s := newRemoteTestServer(t, "")
	v := remoteOn(t, s, RemoteReadOnly)
	c := remoteClient(s, v.Key)
	limited := 0
	for i := 0; i < remoteCallBurst+30; i++ {
		req, _ := http.NewRequest("GET", "http://manager/nodes", nil)
		resp, err := c.RoundTrip(req)
		if err != nil {
			if !strings.Contains(err.Error(), "429") {
				t.Fatal(err)
			}
			limited++
			continue
		}
		resp.Body.Close()
	}
	if limited < 20 {
		t.Fatalf("%d of %d rapid calls were limited", limited, remoteCallBurst+30)
	}
}

func TestRemoteOffSignsOutAndPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "remote-access")
	s := newRemoteTestServer(t, file)
	if v := s.remoteView(); v.Enabled || v.Key != "" {
		t.Fatalf("remote access must start off with no key: %+v", v)
	}
	if code, _ := postJSON(t, s.RemoteHandler(), "/remote/hello", nil); code != http.StatusNotFound {
		t.Fatalf("hello while off: %d", code)
	}
	v := remoteOn(t, s, RemoteStandard)
	if v.URL != "https://relay.example/r/hr1.test/" || v.Key == "" || v.Key == testOperatorToken {
		t.Fatalf("on: %+v", v)
	}
	c := remoteClient(s, v.Key)
	if code, _ := remoteDo(t, c, "GET", "/nodes", ""); code != http.StatusOK {
		t.Fatal(code)
	}

	// A restart keeps it on, with the same key and address.
	again := newRemoteTestServer(t, file)
	if v2 := again.remoteView(); !v2.Enabled || v2.Key != v.Key || v2.URL != v.URL || v2.Mode != RemoteStandard {
		t.Fatalf("after restart: %+v", v2)
	}

	if err := s.SetRemoteAccess(context.Background(), false, "", actorOperatorToken); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://manager/nodes", nil)
	if _, err := c.RoundTrip(req); err == nil {
		t.Fatal("a call worked after remote access was turned off")
	}
	// Rotating the key: the old one opens nothing.
	remoteOn(t, s, RemoteStandard)
	rec := operatorRequest(t, s.NewHTTPHandler(), http.MethodPost, "/remote-access/rotate", testOperatorToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	if _, err := c.RoundTrip(req.Clone(context.Background())); err == nil || !strings.Contains(err.Error(), "not right") {
		t.Fatalf("the old key after rotation: %v", err)
	}
}

// The local dashboard is served exactly as before; only the remote page
// carries the remote client, with a CSP naming exactly its two scripts.
func TestRemotePageAndLocalDashboard(t *testing.T) {
	s := newRemoteTestServer(t, "")
	rec := operatorRequest(t, s.NewHTTPHandler(), http.MethodGet, "/", "", "")
	if !bytes.Equal(rec.Body.Bytes(), dashboardHTML) || rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("the local dashboard changed")
	}
	remoteOn(t, s, RemoteReadOnly)
	page := httptest.NewRecorder()
	s.RemoteHandler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/remote/", nil))
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "window.harnessRemote") || !strings.Contains(body, "Home Compute Harness") {
		t.Fatalf("remote page: %d", page.Code)
	}
	csp := page.Header().Get("Content-Security-Policy")
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(body, -1)
	if len(scripts) != 2 || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("%d scripts, CSP %q", len(scripts), csp)
	}
	for _, m := range scripts {
		sum := sha256.Sum256([]byte(m[1]))
		if !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
			t.Fatal("the CSP does not allow one of the page's scripts")
		}
	}
	if strings.Contains(body, testOperatorToken) || strings.Contains(body, s.remoteView().Key) {
		t.Fatal("the remote page embeds a secret")
	}
}

func TestRemoteNeedsTLSAndTheRelayGateway(t *testing.T) {
	for name, cfg := range map[string]Config{
		"insecure":      {Fingerprint: "", RelayToken: "x", RelayPublicURL: "https://r", EnrollmentPublisher: &fakeRemotePublisher{}},
		"no public URL": {Fingerprint: strings.Repeat("ab", 32), RelayToken: "x"},
	} {
		s := NewServer(nil, nil, cfg)
		if err := s.SetRemoteAccess(context.Background(), true, RemoteStandard, actorOperatorToken); err == nil {
			t.Errorf("%s: remote access turned on", name)
		}
		if v := s.remoteView(); v.Available || v.Unavailable == "" {
			t.Errorf("%s: %+v", name, v)
		}
	}
}
