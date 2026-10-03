package manager

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testOperatorToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDashboardSessionIsDerivedNotTheToken(t *testing.T) {
	session := dashboardSession(testOperatorToken)
	if session == testOperatorToken || session == "" {
		t.Fatalf("session must be derived from, not equal to, the token: %q", session)
	}
	if dashboardSession(testOperatorToken) != session {
		t.Fatal("session must be deterministic so it survives manager restarts")
	}
	if dashboardSession(strings.Repeat("f", 64)) == session {
		t.Fatal("rotating the token must invalidate the old session")
	}
}

func TestLoadOrCreateOperatorToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "operator-token")
	token, err := LoadOrCreateOperatorToken(path)
	if err != nil || len(token) != 64 {
		t.Fatalf("create: token=%q err=%v", token, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Fatalf("token file must be owner-only, got %v", info.Mode().Perm())
		}
	}
	again, err := LoadOrCreateOperatorToken(path)
	if err != nil || again != token {
		t.Fatalf("expected the existing token to be reused, got %q, %v", again, err)
	}
	short := filepath.Join(t.TempDir(), "short")
	os.WriteFile(short, []byte("abc"), 0o600)
	if _, err := LoadOrCreateOperatorToken(short); err == nil {
		t.Fatal("expected a too-short token file to be refused")
	}
}

func operatorRequest(t *testing.T, h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	// A deadline so that, if authentication ever regressed, a streaming
	// route like /events fails this test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Host = "127.0.0.1:7421" // the guard rejects non-local Host names
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestOperatorAuthentication(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken, PairingToken: "permanent-pairing-secret"})
	h := s.NewHTTPHandler()
	session := dashboardSession(testOperatorToken)

	// Everything that reveals or changes anything needs a credential —
	// this is the "another app on the phone" case.
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/nodes", ""},
		{http.MethodGet, "/join-info", ""},
		{http.MethodGet, "/join-script?addr=1.2.3.4:7420&platform=windows", ""},
		{http.MethodGet, "/events", ""},
		{http.MethodGet, "/agent-binaries", ""},
		{http.MethodGet, "/revocations", ""},
		{http.MethodGet, "/workloads", ""},
		{http.MethodPost, "/workloads", `{"command":"calc.exe"}`},
		{http.MethodPost, "/nodes/node-x/revoke", ""},
		{http.MethodPost, "/enrollments", `{"platform":"windows"}`},
		{http.MethodGet, "/enrollments/tok/extra/qr", ""},
		{http.MethodGet, "/join-window", ""},
		{http.MethodPost, "/join-window", `{"minutes":5}`},
		{http.MethodDelete, "/join-window", ""},
		{http.MethodGet, "/join-requests", ""},
	} {
		rec := operatorRequest(t, h, route.method, route.path, "", route.body)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s without a credential: got %d, want 401 with WWW-Authenticate", route.method, route.path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "permanent-pairing-secret") {
			t.Errorf("%s %s leaked the pairing token without a credential", route.method, route.path)
		}
		if rec := operatorRequest(t, h, route.method, route.path, "wrong-token", route.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token: got %d, want 401", route.method, route.path, rec.Code)
		}
	}
	if n := len(s.Workloads.List()); n != 0 {
		t.Fatalf("unauthenticated requests created %d workloads", n)
	}

	// Both credential forms work.
	for name, bearer := range map[string]string{"raw token": testOperatorToken, "dashboard session": session} {
		rec := operatorRequest(t, h, http.MethodGet, "/join-info", bearer, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "permanent-pairing-secret") {
			t.Errorf("GET /join-info with the %s: got %d", name, rec.Code)
		}
	}

	// The bootstrap allow-list works without a credential and leaks nothing.
	if rec := operatorRequest(t, h, http.MethodGet, "/", "", ""); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), testOperatorToken) || strings.Contains(rec.Body.String(), "permanent-pairing-secret") {
		t.Errorf("GET / (dashboard shell): got %d, or it embedded a secret", rec.Code)
	}
	if rec := operatorRequest(t, h, http.MethodGet, "/live", "", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Live") ||
		strings.Contains(rec.Body.String(), testOperatorToken) || strings.Contains(rec.Body.String(), "permanent-pairing-secret") {
		t.Errorf("GET /live (live-view shell): got %d, or it embedded a secret", rec.Code)
	}
	if rec := operatorRequest(t, h, http.MethodGet, "/enrollments/unknown-token/qr", "", ""); rec.Code == http.StatusUnauthorized {
		t.Error("the token-scoped QR route must be reachable by an <img> without a header")
	}
}

// The Android app asks for a proof before it loads the dashboard with the
// login link: only the manager holding the token can answer, nobody needs
// a credential to ask, and no answer works as a credential.
func TestServerProofProvesTheToken(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	h := s.NewHTTPHandler()
	nonce := strings.Repeat("ab", 16)
	rec := operatorRequest(t, h, http.MethodGet, "/server-proof?nonce="+nonce, "", "")
	var resp struct {
		Proof string `json:"proof"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	mac := hmac.New(sha256.New, []byte(testOperatorToken))
	mac.Write([]byte("harness-server-proof-v1:" + nonce))
	if rec.Code != http.StatusOK || resp.Proof != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("server proof: got %d %q", rec.Code, rec.Body.String())
	}
	if other := serverProof(strings.Repeat("f", 64), nonce); other == resp.Proof {
		t.Fatal("a manager with another token must not produce the same proof")
	}
	for _, leaked := range []string{testOperatorToken, dashboardSession(testOperatorToken)} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatal("the proof endpoint returned a credential")
		}
		if rec := operatorRequest(t, h, http.MethodGet, "/nodes", resp.Proof, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a proof worked as a bearer credential: %d", rec.Code)
		}
	}
	for _, bad := range []string{"", "abc", strings.Repeat("AB", 16), strings.Repeat("zz", 16), strings.Repeat("a", 130), "dashboard-session-v1" + strings.Repeat("0", 32)} {
		if rec := operatorRequest(t, h, http.MethodGet, "/server-proof?nonce="+bad, "", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("nonce %q: got %d, want 400", bad, rec.Code)
		}
	}
}

func TestLoginExchangesTokenForSessionWithoutCookie(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	h := s.NewHTTPHandler()

	if rec := operatorRequest(t, h, http.MethodPost, "/login", "", `{"token":"not-it"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with a wrong token: got %d, want 401", rec.Code)
	}
	rec := operatorRequest(t, h, http.MethodPost, "/login", "", `{"token":"`+testOperatorToken+`"}`)
	var resp struct {
		Session string `json:"session"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.Session != dashboardSession(testOperatorToken) {
		t.Fatalf("login: got %d %q", rec.Code, rec.Body.String())
	}
	// A cookie would also reach every other localhost port (RFC 6265
	// §8.5) — any app on the phone listening on one could harvest it.
	if cookie := rec.Header().Get("Set-Cookie"); cookie != "" {
		t.Fatalf("login must not set a cookie, got %q", cookie)
	}
}

func TestOperatorAuthDisabledWithoutToken(t *testing.T) {
	h := NewServer(nil, nil, Config{}).NewHTTPHandler()
	if rec := operatorRequest(t, h, http.MethodGet, "/nodes", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("with no operator token configured the API stays open (tests/embedding), got %d", rec.Code)
	}
}

func TestLoginURL(t *testing.T) {
	if got := LoginURL(":7421", "tok"); got != "http://127.0.0.1:7421/#login=tok" {
		t.Errorf("wildcard api-addr: %q", got)
	}
	if got := LoginURL("127.0.0.1:7421", "tok"); got != "http://127.0.0.1:7421/#login=tok" {
		t.Errorf("explicit api-addr: %q", got)
	}
}
