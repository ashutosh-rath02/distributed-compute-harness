package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

func TestOneTimeEnrollmentAdmitsOneIdentityAndAllowsItsReconnect(t *testing.T) {
	const addr = "127.0.0.1:19320"
	binaryPath, _ := writeDummyAgentBinary(t, []byte("agent binary"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second,
		AgentBinaries: dummyWindowsBuild(binaryPath), EnrollmentTTL: time.Minute,
	})
	transport.Handle("/enroll/", srv.EnrollmentHandler())
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()

	request := []byte(`{"addr":"127.0.0.1:19320","platform":"windows"}`)
	resp, err := http.Post(api.URL+"/enrollments", "application/json", bytes.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var invite struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&invite); err != nil || invite.Token == "" {
		t.Fatalf("decode invitation: token=%q err=%v", invite.Token, err)
	}

	identityDir := filepath.Join(t.TempDir(), "enrolled-node")
	firstCtx, stopFirst := context.WithCancel(context.Background())
	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr: addr, PairingToken: invite.Token, IdentityDir: identityDir,
		Name: "enrolled-node", HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond,
		Insecure: true, InsecureWorkloadsDisabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(firstCtx)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	landingResp, err := http.Get(invite.URL)
	if err != nil {
		t.Fatalf("GET consumed enrollment: %v", err)
	}
	landingResp.Body.Close()
	if landingResp.StatusCode != http.StatusNotFound {
		t.Fatalf("consumed enrollment page status=%d, want 404", landingResp.StatusCode)
	}
	stopFirst()
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeOffline
	})

	secondCtx, stopSecond := context.WithCancel(context.Background())
	defer stopSecond()
	a2, err := agent.New(ws.New(), agent.Config{
		ManagerAddr: addr, PairingToken: invite.Token, IdentityDir: identityDir,
		Name: "enrolled-node", HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond,
		Insecure: true, InsecureWorkloadsDisabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a2.Run(secondCtx)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a2.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	otherCtx, stopOther := context.WithCancel(context.Background())
	defer stopOther()
	other, err := agent.New(ws.New(), agent.Config{
		ManagerAddr: addr, PairingToken: invite.Token, IdentityDir: filepath.Join(t.TempDir(), "other-node"),
		Name: "other-node", HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond,
		Insecure: true, InsecureWorkloadsDisabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go other.Run(otherCtx)
	time.Sleep(300 * time.Millisecond)
	if _, ok := srv.Registry.Get(other.NodeID()); ok {
		t.Fatal("consumed enrollment token admitted a second identity")
	}
}
