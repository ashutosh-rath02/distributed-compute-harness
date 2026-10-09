package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// TestRolesOnARealManager runs a real manager (database, agent) and drives
// it as the three roles over HTTP: a viewer reads, an operator also runs
// work, an admin also revokes — and what each did is attributed by name.
func TestRolesOnARealManager(t *testing.T) {
	const addr = "127.0.0.1:19740"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := persistent.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := manager.NewServer(ws.New(), store, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, OperatorToken: operatorToken,
	})
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	a := startRegisteredAgent(t, addr, "roles-agent")
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	tokens := map[string]string{}
	for _, role := range []string{"viewer", "operator", "admin"} {
		resp := operatorCall(t, http.MethodPost, api.URL+"/users", operatorToken, `{"name":"`+role+`-user","role":"`+role+`"}`)
		var out struct {
			Token string `json:"token"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated || len(out.Token) != 64 {
			t.Fatalf("add %s: %d", role, resp.StatusCode)
		}
		tokens[role] = out.Token
	}

	status := func(method, path, role, body string) int {
		resp := operatorCall(t, method, api.URL+path, tokens[role], body)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	id := string(a.NodeID())
	work := `{"capability":"cpu.burn","params":{"seconds":"1"}}`

	if c := status("GET", "/nodes", "viewer", ""); c != 200 {
		t.Fatalf("viewer GET /nodes: %d", c)
	}
	for _, p := range []string{"/join-info", "/ai-info", "/join-requests", "/users", "/remote-access"} {
		if c := status("GET", p, "viewer", ""); c != 403 {
			t.Errorf("viewer GET %s: %d, want 403", p, c)
		}
		if c := status("GET", p, "operator", ""); c != 403 {
			t.Errorf("operator GET %s: %d, want 403", p, c)
		}
	}
	if c := status("POST", "/workloads", "viewer", work); c != 403 {
		t.Errorf("viewer submitted work: %d", c)
	}
	if c := status("POST", "/workloads", "operator", work); c == 403 || c >= 500 {
		t.Errorf("operator submit: %d", c)
	}
	if c := status("POST", "/nodes/"+id+"/revoke", "operator", ""); c != 403 {
		t.Errorf("operator revoked a node: %d", c)
	}
	if c := status("PUT", "/policy", "operator", `{}`); c != 403 {
		t.Errorf("operator changed policy: %d", c)
	}
	if c := status("POST", "/nodes/"+id+"/revoke", "admin", ""); c != 200 {
		t.Errorf("admin revoke: %d", c)
	}

	entries, _ := srv.AuditEntries(domain.AuditSecurity, 20)
	var sawAdmin bool
	for _, e := range entries {
		if strings.Contains(e.Kind, "revoke") && e.Actor == "admin-user" {
			sawAdmin = true
		}
	}
	if !sawAdmin {
		t.Errorf("the revocation is not attributed to admin-user: %+v", entries)
	}
	// Revoking the user kills its token at once.
	operatorCall(t, "DELETE", api.URL+"/users/viewer-user", operatorToken, "").Body.Close()
	if c := status("GET", "/nodes", "viewer", ""); c != 401 {
		t.Errorf("removed viewer: %d", c)
	}
}
