package integration

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

const operatorToken = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"

func operatorCall(t *testing.T, method, url, bearer, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// TestOperatorAPIRequiresTheOperatorToken runs a real manager + agent with
// authentication on: the token holder (harnessctl's bearer) can do
// everything including stream events, while a client without it — any
// other app on the phone, which shares loopback — can do nothing.
func TestOperatorAPIRequiresTheOperatorToken(t *testing.T) {
	const addr = "127.0.0.1:19506"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := manager.NewServer(ws.New(), nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, OperatorToken: operatorToken,
	})
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	a := startRegisteredAgent(t, addr, "auth-agent")
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	// Another app on the phone: no credential.
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, "/join-info", ""},
		{http.MethodGet, "/nodes", ""},
		{http.MethodPost, "/workloads", `{"command":"cmd","args":["/C","echo","from-another-app"]}`},
		{http.MethodPost, "/nodes/" + string(a.NodeID()) + "/revoke", ""},
	} {
		resp := operatorCall(t, call.method, api.URL+call.path, "", call.body)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), pairingToken) {
			t.Errorf("unauthenticated %s %s: got %d %s", call.method, call.path, resp.StatusCode, body)
		}
	}
	if n := len(srv.Workloads.List()); n != 0 {
		t.Fatalf("an unauthenticated client created %d workloads", n)
	}
	if _, ok := srv.Registry.Get(a.NodeID()); !ok {
		t.Fatal("an unauthenticated client revoked a node")
	}

	// The token holder: stream events, then cause one.
	stream := operatorCall(t, http.MethodGet, api.URL+"/events", operatorToken, "")
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("GET /events with the operator token: %d", stream.StatusCode)
	}
	sawEvent := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "data: ") && strings.Contains(line, "workload.") {
				sawEvent <- line
				return
			}
		}
	}()

	cmd, args := echoArgs("from-the-operator")
	body := `{"command":"` + cmd + `","args":["` + strings.Join(args, `","`) + `"]}`
	resp := operatorCall(t, http.MethodPost, api.URL+"/workloads", operatorToken, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /workloads with the operator token: %d", resp.StatusCode)
	}
	select {
	case <-sawEvent:
	case <-time.After(5 * time.Second):
		t.Fatal("expected the authenticated event stream to deliver the workload's events")
	}

	// The dashboard's derived session works as a bearer too.
	login := operatorCall(t, http.MethodPost, api.URL+"/login", "", `{"token":"`+operatorToken+`"}`)
	loginBody, _ := io.ReadAll(login.Body)
	login.Body.Close()
	session := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(string(loginBody)), `{"session":"`), `"}`)
	if login.StatusCode != http.StatusOK || session == "" || session == operatorToken || login.Header.Get("Set-Cookie") != "" {
		t.Fatalf("login: %d %s (Set-Cookie=%q)", login.StatusCode, loginBody, login.Header.Get("Set-Cookie"))
	}
	resp = operatorCall(t, http.MethodGet, api.URL+"/join-info", session, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /join-info with the dashboard session: %d", resp.StatusCode)
	}
}
