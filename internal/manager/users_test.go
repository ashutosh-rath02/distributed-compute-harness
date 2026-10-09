package manager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/store/persistent"
)

func doJSON(t *testing.T, h http.Handler, method, path, bearer, body string) (int, map[string]any) {
	t.Helper()
	rec := operatorRequest(t, h, method, path, bearer, body)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestUserTokenIsStoredOnlyAsAHash(t *testing.T) {
	store, err := persistent.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewServer(nil, store, Config{OperatorToken: testOperatorToken})
	tok, err := s.AddUser("alice", domain.RoleOperator, time.Time{})
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q err %v", tok, err)
	}
	stored, _ := store.ListUsers()
	if len(stored) != 1 || stored[0].TokenHash != hashToken(tok) || strings.Contains(stored[0].TokenHash, tok) {
		t.Fatalf("stored: %+v", stored)
	}
	raw, _ := json.Marshal(stored)
	if strings.Contains(string(raw), tok) {
		t.Fatal("the token itself is in the store")
	}
	// A new manager over the same store knows alice.
	s2 := NewServer(nil, store, Config{OperatorToken: testOperatorToken})
	if u, ok := s2.userByToken(tok, time.Now()); !ok || u.Name != "alice" {
		t.Fatal("user did not survive a restart")
	}
	if _, ok := s2.userByToken(tok[:63]+"0", time.Now()); ok {
		t.Fatal("a wrong token authenticated")
	}
}

func TestUserNamesAreValidated(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	for _, bad := range []string{"", "Alice", "a b", "1abc", "node", "anonymous", "operator-token", "remote", strings.Repeat("a", 33), "a/b", "<b>"} {
		if _, err := s.AddUser(bad, domain.RoleViewer, time.Time{}); err == nil {
			t.Errorf("name %q was accepted", bad)
		}
	}
	if _, err := s.AddUser("ok", "root", time.Time{}); err == nil {
		t.Error("an unknown role was accepted")
	}
	if _, err := s.AddUser("ok", domain.RoleViewer, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("ok", domain.RoleViewer, time.Time{}); err != errUserExists {
		t.Errorf("duplicate: %v", err)
	}
}

func TestUserExpiryAndGraceRotation(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	tok, _ := s.AddUser("bob", domain.RoleViewer, time.Now().Add(time.Hour))
	if _, ok := s.userByToken(tok, time.Now()); !ok {
		t.Fatal("fresh user refused")
	}
	if _, ok := s.userByToken(tok, time.Now().Add(2*time.Hour)); ok {
		t.Fatal("an expired user was accepted")
	}
	// rotate with grace: both work, then only the new one
	fresh, err := s.ReissueUserToken("bob", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.userByToken(tok, time.Now()); !ok {
		t.Error("old token should work during grace")
	}
	if _, ok := s.userByToken(tok, time.Now().Add(11*time.Minute)); ok {
		t.Error("old token should stop after grace")
	}
	if _, ok := s.userByToken(fresh, time.Now()); !ok {
		t.Error("new token refused")
	}
	// reissue without grace: old stops at once
	third, _ := s.ReissueUserToken("bob", 0)
	if _, ok := s.userByToken(fresh, time.Now()); ok {
		t.Error("replaced token still works")
	}
	if _, ok := s.userByToken(third, time.Now()); !ok {
		t.Error("newest token refused")
	}
}

func TestSessionsCarryTheUserAndDieOnRotateOrRemove(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	h := s.NewHTTPHandler()
	tok, _ := s.AddUser("carol", domain.RoleViewer, time.Time{})

	code, out := doJSON(t, h, "POST", "/login", "", `{"token":"`+tok+`"}`)
	session, _ := out["session"].(string)
	if code != 200 || out["role"] != "viewer" || out["user"] != "carol" || strings.Contains(session, tok) {
		t.Fatalf("login: %d %v", code, out)
	}
	if code, out := doJSON(t, h, "GET", "/whoami", session, ""); code != 200 || out["user"] != "carol" || out["role"] != "viewer" {
		t.Fatalf("whoami: %d %v", code, out)
	}
	if code, _ := doJSON(t, h, "POST", "/workloads", session, `{}`); code != 403 {
		t.Fatalf("a viewer session submitted work: %d", code)
	}
	// role changes apply at once (rewrite the record)
	s.users.mu.Lock()
	s.users.users["carol"].Role = domain.RoleOperator
	s.users.mu.Unlock()
	if code, _ := doJSON(t, h, "POST", "/workloads", session, `{}`); code == 403 {
		t.Fatal("a promoted user is still forbidden")
	}
	// a session forged without the operator-token-derived key
	forged := signSession(strings.Repeat("e", 64), "carol", s.users.users["carol"].Epoch, time.Now().Add(time.Hour))
	if code, _ := doJSON(t, h, "GET", "/nodes", forged, ""); code != 401 {
		t.Fatalf("forged session: %d", code)
	}
	// an expired one
	expired := signSession(testOperatorToken, "carol", s.users.users["carol"].Epoch, time.Now().Add(-time.Second))
	if code, _ := doJSON(t, h, "GET", "/nodes", expired, ""); code != 401 {
		t.Fatalf("expired session: %d", code)
	}
	// rotating signs the browser out
	if _, err := s.ReissueUserToken("carol", time.Hour); err != nil {
		t.Fatal(err)
	}
	if code, _ := doJSON(t, h, "GET", "/nodes", session, ""); code != 401 {
		t.Fatalf("session survived a token change: %d", code)
	}
	// removing signs out too, and kills the token
	_, out = doJSON(t, h, "POST", "/login", "", `{"token":"`+testOperatorToken+`"}`)
	admin := out["session"].(string)
	if code, _ := doJSON(t, h, "GET", "/nodes", admin, ""); code != 200 {
		t.Fatalf("admin session: %d", code)
	}
	tok2, _ := s.AddUser("dave", domain.RoleViewer, time.Time{})
	_, out = doJSON(t, h, "POST", "/login", "", `{"token":"`+tok2+`"}`)
	sess2 := out["session"].(string)
	if code, _ := doJSON(t, h, "DELETE", "/users/dave", admin, ""); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	if code, _ := doJSON(t, h, "GET", "/nodes", sess2, ""); code != 401 {
		t.Fatalf("removed user's session: %d", code)
	}
	if code, _ := doJSON(t, h, "GET", "/nodes", tok2, ""); code != 401 {
		t.Fatalf("removed user's token: %d", code)
	}
}

func TestAuditRecordsTheUserAsActor(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	h := s.NewHTTPHandler()
	tok, _ := s.AddUser("erin", domain.RoleAdmin, time.Time{})
	if code, _ := doJSON(t, h, "POST", "/users", tok, `{"name":"frank","role":"viewer"}`); code != 201 {
		t.Fatalf("add: %d", code)
	}
	entries, _ := s.AuditEntries(domain.AuditSecurity, 5)
	found := false
	for _, e := range entries {
		if e.Kind == "user.added" && e.Actor == "erin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no user.added by erin: %+v", entries)
	}
	// The operator token keeps its own actor.
	doJSON(t, h, "POST", "/users", testOperatorToken, `{"name":"gina","role":"viewer"}`)
	entries, _ = s.AuditEntries(domain.AuditSecurity, 1)
	if entries[0].Actor != actorOperatorToken {
		t.Fatalf("actor %q", entries[0].Actor)
	}
}

func TestUsersAPINeverReturnsSecrets(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	h := s.NewHTTPHandler()
	tok, _ := s.AddUser("hank", domain.RoleViewer, time.Time{})
	rec := operatorRequest(t, h, "GET", "/users", testOperatorToken, "")
	if strings.Contains(rec.Body.String(), tok) || strings.Contains(rec.Body.String(), hashToken(tok)) || !strings.Contains(rec.Body.String(), "hank") {
		t.Fatalf("list: %s", rec.Body)
	}
}

func TestFailedAttemptsGetA429ButValidCredentialsStillWork(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	h := s.NewHTTPHandler()
	call := func(bearer string) int {
		req := httptest.NewRequest("GET", "/nodes", nil)
		req.Host = "127.0.0.1:7421"
		req.RemoteAddr = "127.0.0.1:4444"
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < authFailLimit; i++ {
		if c := call("wrong"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c := call("wrong"); c != 429 {
		t.Fatalf("over the limit: %d", c)
	}
	if c := call(testOperatorToken); c != 200 {
		t.Fatalf("a valid token was refused while the source was throttled: %d", c)
	}
	// bounded memory
	var l authLimiter
	now := time.Now()
	for i := 0; i < 5000; i++ {
		l.tooMany("10.0.0."+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune(i)), now)
	}
	if len(l.entries) > authFailKeys {
		t.Fatalf("limiter grew to %d", len(l.entries))
	}
	// the window resets
	var l2 authLimiter
	for i := 0; i <= authFailLimit; i++ {
		l2.tooMany("s", now)
	}
	if l2.tooMany("s", now.Add(authFailWindow+time.Second)) {
		t.Fatal("still limited after the window")
	}
}

func TestRotateSecrets(t *testing.T) {
	dir := t.TempDir()
	opFile := filepath.Join(dir, "operator-token")
	os.WriteFile(opFile, []byte(testOperatorToken+"\n"), 0o600)
	store, _ := persistent.Open(filepath.Join(dir, "m.db"))
	defer store.Close()
	cfg := Config{OperatorToken: testOperatorToken, AIKey: strings.Repeat("a", 64), PairingToken: "old-pairing",
		OperatorTokenFile: opFile, AIKeyFile: filepath.Join(dir, "ai-key"), PairingTokenFile: filepath.Join(dir, "pairing-token")}
	s := NewServer(nil, store, cfg)
	h := s.NewHTTPHandler()

	_, out := doJSON(t, h, "POST", "/rotate/pairing-token", testOperatorToken, "")
	newPair, _ := out["value"].(string)
	if len(newPair) != 64 || s.pairingToken() != newPair || s.pairingMatches("old-pairing") || !s.pairingMatches(newPair) {
		t.Fatalf("pairing rotate: %v", out)
	}
	if data, _ := os.ReadFile(cfg.PairingTokenFile); strings.TrimSpace(string(data)) != newPair {
		t.Fatal("pairing token file not updated")
	}
	// survives a restart even if the flag still holds the old value
	if s2 := NewServer(nil, store, cfg); s2.pairingToken() != newPair {
		t.Fatal("rotated pairing token lost at restart")
	}

	_, out = doJSON(t, h, "POST", "/rotate/ai-key", testOperatorToken, "")
	newAI := out["value"].(string)
	if s.aiKey() != newAI {
		t.Fatal("ai key not rotated")
	}
	if code, _ := doJSON(t, h, "GET", "/v1/models", cfg.AIKey, ""); code != 401 {
		t.Fatalf("old AI key: %d", code)
	}
	if code, _ := doJSON(t, h, "GET", "/v1/models", newAI, ""); code != 200 {
		t.Fatalf("new AI key: %d", code)
	}

	_, login := doJSON(t, h, "POST", "/login", "", `{"token":"`+testOperatorToken+`"}`)
	oldSession := login["session"].(string)
	_, out = doJSON(t, h, "POST", "/rotate/operator-token", testOperatorToken, "")
	newOp := out["value"].(string)
	if data, _ := os.ReadFile(opFile); strings.TrimSpace(string(data)) != newOp {
		t.Fatal("operator token file not updated")
	}
	for _, old := range []string{testOperatorToken, oldSession, dashboardSession(testOperatorToken)} {
		if code, _ := doJSON(t, h, "GET", "/nodes", old, ""); code != 401 {
			t.Fatalf("old operator credential still works: %d", code)
		}
	}
	if code, _ := doJSON(t, h, "GET", "/nodes", newOp, ""); code != 200 {
		t.Fatalf("new operator token: %d", code)
	}
	// users' own tokens are unaffected
	tok, _ := s.AddUser("ivy", domain.RoleViewer, time.Time{})
	if code, _ := doJSON(t, h, "GET", "/nodes", tok, ""); code != 200 {
		t.Fatalf("user token: %d", code)
	}
	if code, _ := doJSON(t, h, "POST", "/rotate/bogus", newOp, ""); code != 404 {
		t.Fatalf("bogus rotate: %d", code)
	}
	if code, _ := doJSON(t, h, "POST", "/rotate/ai-key", tok, ""); code != 403 {
		t.Fatalf("viewer rotated a key: %d", code)
	}
}

func TestEmptyPairingTokenNeverMatches(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	if s.pairingMatches("") {
		t.Fatal("an empty pairing token matched an empty configuration")
	}
}
