package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var testFingerprint = strings.Repeat("ab", 32)

func TestRemoteTokenIsOpaqueAndNotADeviceAlias(t *testing.T) {
	srv := NewServerWithAliasKey(0, "stable-alias-secret")
	token, err := sealRemote(srv.aliasCipher, remotePayload{Session: "private-manager-session", Fingerprint: testFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, remotePrefix) || strings.Contains(token, "private-manager-session") {
		t.Fatalf("token %q is not opaque", token)
	}
	if p, ok := openRemote(srv.aliasCipher, token); !ok || p.Session != "private-manager-session" || p.Fingerprint != testFingerprint {
		t.Fatalf("token did not round-trip: %+v %v", p, ok)
	}
	restarted := NewServerWithAliasKey(0, "stable-alias-secret")
	if _, ok := openRemote(restarted.aliasCipher, token); !ok {
		t.Fatal("token did not survive a relay restart with the same alias key")
	}
	// A dashboard address is not a rendezvous credential...
	if got := srv.resolveSession(token); got != token {
		t.Fatal("a remote dashboard token resolved as a device alias")
	}
	// ...and a device alias is not a dashboard address.
	alias, _ := sealAlias(srv.aliasCipher, "private-manager-session")
	if _, ok := openRemote(srv.aliasCipher, alias); ok {
		t.Fatal("a device alias opened as a remote dashboard token")
	}
	if _, ok := openRemote(srv.aliasCipher, remotePrefix+alias[len(aliasPrefix):]); ok {
		t.Fatal("a device alias with the prefix swapped opened as a remote dashboard token")
	}
	tampered := []byte(token)
	tampered[len(tampered)/2] ^= 0x01
	if _, ok := openRemote(srv.aliasCipher, string(tampered)); ok {
		t.Fatal("a tampered token was accepted")
	}
}

func TestPublishRemoteDashboard(t *testing.T) {
	gateway := NewPublicGateway(NewServerWithAliasKey(0, "alias-key"), "127.0.0.1:1", "correct-secret")
	publish := func(bearer string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/_harness/remote-dashboards", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := publish("wrong-secret", map[string]string{"session": "s", "fingerprint": testFingerprint}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong publish secret: %d", rec.Code)
	}
	if rec := publish("correct-secret", map[string]string{"session": "s"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no fingerprint (an -insecure manager): %d", rec.Code)
	}
	rec := publish("correct-secret", map[string]string{"session": "s", "fingerprint": testFingerprint})
	var out struct{ Path string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !strings.HasPrefix(out.Path, "/r/"+remotePrefix) || !strings.HasSuffix(out.Path, "/") {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}

	get := func(method, path string) int {
		rec := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code
	}
	// Only the dashboard's own endpoints are forwarded, each with its one
	// method; unknown tokens are not found. (Nothing listens at the relay
	// address here, so a forwarded request is a 502.)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/r/" + remotePrefix + "bogus/", http.StatusNotFound},
		{http.MethodGet, out.Path + "anything", http.StatusNotFound},
		{http.MethodGet, out.Path + "call", http.StatusMethodNotAllowed},
		{http.MethodPost, out.Path, http.StatusMethodNotAllowed},
		{http.MethodGet, strings.TrimSuffix(out.Path, "/"), http.StatusFound},
	} {
		if got := get(c.method, c.path); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
}
