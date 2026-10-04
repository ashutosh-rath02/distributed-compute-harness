package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	"home-harness/internal/protocol"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// Joining by approval (agent -pair, the join page's installers): a device
// with no token waits as a join request until the operator approves it,
// comparing the pairing code both sides compute.

func startPairingManager(t *testing.T, addr, token string) (*manager.Server, string, string) {
	t.Helper()
	return startPairingManagerWindow(t, addr, token, time.Minute)
}

// startPairingManagerWindow starts a TLS manager with a store and an API;
// firstRun is how long adding devices is open at its (first) start.
func startPairingManagerWindow(t *testing.T, addr, token string, firstRun time.Duration) (*manager.Server, string, string) {
	t.Helper()
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	store, err := persistent.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatalf("persistent.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fp := mtls.Fingerprint(cert)
	srv := manager.NewServer(ws.NewTLSServer(cert), store, manager.Config{
		Addr: addr, PairingToken: token, Fingerprint: fp, HeartbeatTimeout: 2 * time.Second,
		FirstRunJoinWindow: firstRun,
	})
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	return srv, fp, api.URL
}

func startPairingAgent(t *testing.T, ctx context.Context, addr, fingerprint, dir string) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.NewTLSClient(mtls.PinnedClientConfig(fingerprint)), agent.Config{DeviceUse: pluggedIn,
		ManagerAddr: addr, Pairing: true, ManagerFingerprint: fingerprint,
		IdentityDir: dir, Name: "pairing-laptop", HostFingerprint: "-",
		HeartbeatInterval: 100 * time.Millisecond, PairingRetry: 100 * time.Millisecond, JoinClosedRetry: 100 * time.Millisecond,
		ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a
}

func joinRequests(t *testing.T, api string) []manager.JoinRequest {
	t.Helper()
	resp, err := http.Get(api + "/join-requests")
	if err != nil {
		t.Fatalf("GET /join-requests: %v", err)
	}
	defer resp.Body.Close()
	var reqs []manager.JoinRequest
	if err := json.NewDecoder(resp.Body).Decode(&reqs); err != nil {
		t.Fatalf("decode join requests: %v", err)
	}
	return reqs
}

func decideJoin(t *testing.T, api string, id domain.NodeID, verb string) int {
	t.Helper()
	resp, err := http.Post(api+"/join-requests/"+string(id)+"/"+verb, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", verb, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func isReady(srv *manager.Server, id domain.NodeID) bool {
	rec, ok := srv.Registry.Get(id)
	return ok && rec.State == domain.NodeReady
}

func TestPairingDeviceJoinsOnlyAfterApproval(t *testing.T) {
	const addr = "127.0.0.1:19580"
	srv, fp, api := startPairingManager(t, addr, "the-shared-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := filepath.Join(t.TempDir(), "laptop")
	a := startPairingAgent(t, ctx, addr, fp, dir)

	waitFor(t, 3*time.Second, func() bool { return len(joinRequests(t, api)) == 1 })
	req := joinRequests(t, api)[0]
	if req.NodeID != a.NodeID() || req.Name != "pairing-laptop" || req.Code != a.PairingCode() {
		t.Fatalf("request %+v; device %s shows code %s", req, a.NodeID(), a.PairingCode())
	}
	// It keeps asking, but stays one request and stays out.
	time.Sleep(600 * time.Millisecond)
	if reqs := joinRequests(t, api); len(reqs) != 1 || isReady(srv, a.NodeID()) {
		t.Fatalf("before approval: %d requests, ready=%v", len(reqs), isReady(srv, a.NodeID()))
	}

	if code := decideJoin(t, api, a.NodeID(), "approve"); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	waitFor(t, 3*time.Second, func() bool { return isReady(srv, a.NodeID()) })
	if reqs := joinRequests(t, api); len(reqs) != 0 {
		t.Fatalf("an admitted device must leave the list, got %+v", reqs)
	}

	// The audit log says how it got in, and who let it. (The admission is
	// recorded just after the node turns READY: poll.)
	auditHas := func() (admitted, approved bool) {
		resp, err := http.Get(api + "/audit")
		if err != nil {
			t.Fatal(err)
		}
		var entries []domain.AuditEntry
		json.NewDecoder(resp.Body).Decode(&entries)
		resp.Body.Close()
		for _, e := range entries {
			admitted = admitted || (e.Kind == "node.admitted" && e.Detail["by"] == "approval:"+req.Code)
			approved = approved || (e.Kind == "node.join-approved" && e.NodeID == a.NodeID())
		}
		return admitted, approved
	}
	waitFor(t, 3*time.Second, func() bool { admitted, approved := auditHas(); return admitted && approved })

	// Later starts reconnect by key: no new request.
	cancel()
	waitFor(t, 3*time.Second, func() bool { return !isReady(srv, a.NodeID()) })
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	startPairingAgent(t, ctx2, addr, fp, dir)
	waitFor(t, 3*time.Second, func() bool { return isReady(srv, a.NodeID()) })
	if reqs := joinRequests(t, api); len(reqs) != 0 {
		t.Fatalf("a known device must not ask again, got %+v", reqs)
	}
}

func TestPairingRejectedDeviceStaysOut(t *testing.T) {
	const addr = "127.0.0.1:19581"
	srv, fp, api := startPairingManager(t, addr, "the-shared-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startPairingAgent(t, ctx, addr, fp, filepath.Join(t.TempDir(), "stranger"))
	waitFor(t, 3*time.Second, func() bool { return len(joinRequests(t, api)) == 1 })
	if code := decideJoin(t, api, a.NodeID(), "reject"); code != http.StatusOK {
		t.Fatalf("reject: %d", code)
	}
	time.Sleep(800 * time.Millisecond)
	if isReady(srv, a.NodeID()) || len(joinRequests(t, api)) != 0 {
		t.Fatal("a rejected device must neither get in nor come back as a request")
	}
	if code := decideJoin(t, api, a.NodeID(), "approve"); code != http.StatusNotFound {
		t.Fatalf("approving a rejected device that isn't asking: %d, want 404", code)
	}
}

// Pairing devices send no token. A manager configured with an empty
// pairing token (tests, embedding) must still hold them for approval.
func TestPairingNeverAdmittedByAnEmptyPairingToken(t *testing.T) {
	const addr = "127.0.0.1:19582"
	srv, fp, api := startPairingManager(t, addr, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startPairingAgent(t, ctx, addr, fp, filepath.Join(t.TempDir(), "laptop"))
	waitFor(t, 3*time.Second, func() bool { return len(joinRequests(t, api)) == 1 })
	time.Sleep(500 * time.Millisecond)
	if isReady(srv, a.NodeID()) {
		t.Fatal("a pairing device got in without approval")
	}
}

func rawPairingRegister(t *testing.T, addr, dir string) (*protocol.Envelope, domain.NodeID) {
	t.Helper()
	id, err := identity.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("identity.LoadOrCreate: %v", err)
	}
	payload := protocol.RegisterPayload{
		Manifest:  domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Node: domain.Node{Identity: id.Identity, Name: "raw-" + filepath.Base(dir)}},
		Signature: id.Sign(protocol.RegisterSignedData("", id.NodeID)),
		Pairing:   true,
	}
	return rawRegister(t, addr, payload, id.NodeID), id.NodeID
}

func rejectPayload(t *testing.T, env *protocol.Envelope) protocol.RegisterRejectPayload {
	t.Helper()
	if env.Type != protocol.MsgRegisterReject {
		t.Fatalf("expected REGISTER_REJECT, got %s", env.Type)
	}
	var p protocol.RegisterRejectPayload
	if err := env.DecodePayload(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Asking to join is no way back in for a revoked identity.
func TestPairingRefusesARevokedIdentity(t *testing.T) {
	const addr = "127.0.0.1:19583"
	_, api := startManagerWithAPI(t, addr, 2*time.Second)
	dir := filepath.Join(t.TempDir(), "revoked")
	if resp := registerAs(t, addr, dir, pairingToken); resp.Type != protocol.MsgRegisterAck {
		t.Fatalf("first registration: %s", resp.Type)
	}
	id, _ := identity.LoadOrCreate(dir)
	resp, err := http.Post(api.URL+"/nodes/"+string(id.NodeID)+"/revoke", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %v %v", err, resp)
	}
	resp.Body.Close()
	env, _ := rawPairingRegister(t, addr, dir)
	if p := rejectPayload(t, env); p.PendingApproval || p.Reason != "node revoked by operator" {
		t.Fatalf("a revoked identity asking to join: %+v", p)
	}
	if reqs := joinRequests(t, api.URL); len(reqs) != 0 {
		t.Fatalf("a revoked identity must not become a request: %+v", reqs)
	}
}

// Anyone on the network may ask, so one host can't crowd out the rest.
func TestPairingRequestsFromOneHostAreCapped(t *testing.T) {
	const addr = "127.0.0.1:19584"
	_, api := startManagerWithAPI(t, addr, 2*time.Second)
	openJoinWindow(t, api.URL)
	for i := 0; i < 4; i++ {
		env, _ := rawPairingRegister(t, addr, filepath.Join(t.TempDir(), fmt.Sprintf("flood-%d", i)))
		p := rejectPayload(t, env)
		if i < 3 && (!p.PendingApproval || p.Code == "") {
			t.Fatalf("request %d: %+v", i, p)
		}
		if i == 3 && p.PendingApproval {
			t.Fatalf("a fourth request from one host must be refused, got %+v", p)
		}
	}
	if n := len(joinRequests(t, api.URL)); n != 3 {
		t.Fatalf("want 3 requests, got %d", n)
	}
}

func openJoinWindow(t *testing.T, api string) {
	t.Helper()
	resp, err := http.Post(api+"/join-window", "application/json", strings.NewReader(`{"minutes":5}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("open the join window: %v %v", err, resp)
	}
	resp.Body.Close()
}

func closeJoinWindow(t *testing.T, api string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, api+"/join-window", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("close the join window: %v %v", err, resp)
	}
	resp.Body.Close()
}

// While adding devices is closed, a new device can't even queue a request;
// it waits quietly and gets in once someone opens the window and approves.
func TestPairingWaitsForTheJoinWindow(t *testing.T) {
	const addr = "127.0.0.1:19585"
	srv, fp, api := startPairingManagerWindow(t, addr, "the-shared-token", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startPairingAgent(t, ctx, addr, fp, filepath.Join(t.TempDir(), "late"))
	time.Sleep(800 * time.Millisecond)
	if n := len(joinRequests(t, api)); n != 0 || isReady(srv, a.NodeID()) {
		t.Fatalf("closed window: %d requests, ready=%v", n, isReady(srv, a.NodeID()))
	}
	openJoinWindow(t, api)
	waitFor(t, 3*time.Second, func() bool { return len(joinRequests(t, api)) == 1 })
	if code := decideJoin(t, api, a.NodeID(), "approve"); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	waitFor(t, 3*time.Second, func() bool { return isReady(srv, a.NodeID()) })
}

// Closing the window must not strand a device that was already waiting.
func TestApprovalAfterTheJoinWindowCloses(t *testing.T) {
	const addr = "127.0.0.1:19586"
	srv, fp, api := startPairingManager(t, addr, "the-shared-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startPairingAgent(t, ctx, addr, fp, filepath.Join(t.TempDir(), "early"))
	waitFor(t, 3*time.Second, func() bool { return len(joinRequests(t, api)) == 1 })
	closeJoinWindow(t, api)
	time.Sleep(300 * time.Millisecond)
	if len(joinRequests(t, api)) != 1 {
		t.Fatal("a waiting device lost its request when the window closed")
	}
	if code := decideJoin(t, api, a.NodeID(), "approve"); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	waitFor(t, 3*time.Second, func() bool { return isReady(srv, a.NodeID()) })
}

// A manager that already knows devices starts with adding devices closed:
// restarting it must not reopen the window.
func TestJoinWindowStartsClosedForAKnownFleet(t *testing.T) {
	const addr = "127.0.0.1:19587"
	dbPath := filepath.Join(t.TempDir(), "manager.db")
	store, err := persistent.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := identity.LoadOrCreate(filepath.Join(t.TempDir(), "known"))
	if err := store.UpsertNode(domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Node: domain.Node{Identity: id.Identity, Name: "known"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); store.Close() })
	srv := manager.NewServer(ws.New(), store, manager.Config{Addr: addr, PairingToken: "x", HeartbeatTimeout: 2 * time.Second, FirstRunJoinWindow: time.Hour})
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	var v struct{ Open bool }
	waitFor(t, 3*time.Second, func() bool {
		resp, err := http.Get(api.URL + "/join-window")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(&v) == nil && srv.Registry.List() != nil && len(srv.Registry.List()) == 1
	})
	if v.Open {
		t.Fatal("a manager that knows devices must start with adding devices closed")
	}
}
