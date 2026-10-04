package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// registerAs sends a REGISTER signed by the identity persisted in dir,
// presenting token, and returns the manager's reply — the same proof a
// real agent with that identity directory would present.
func registerAs(t *testing.T, addr, dir, token string) *protocol.Envelope {
	t.Helper()
	id, err := identity.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("identity.LoadOrCreate: %v", err)
	}
	payload := protocol.RegisterPayload{
		Manifest: domain.Manifest{
			SchemaVersion: domain.ManifestSchemaVersion,
			Node:          domain.Node{Identity: id.Identity, Name: "raw-" + filepath.Base(dir)},
		},
		PairingToken: token,
		Signature:    id.Sign(protocol.RegisterSignedData(token, id.NodeID)),
	}
	return rawRegister(t, addr, payload, id.NodeID)
}

func expectRevokedReject(t *testing.T, resp *protocol.Envelope) {
	t.Helper()
	if resp.Type != protocol.MsgRegisterReject {
		t.Fatalf("expected REGISTER_REJECT for a revoked identity, got %s", resp.Type)
	}
	var reject protocol.RegisterRejectPayload
	if err := resp.DecodePayload(&reject); err != nil {
		t.Fatalf("decode reject: %v", err)
	}
	if reject.Reason != "node revoked by operator" {
		t.Fatalf("expected the distinct revocation reason, got %q", reject.Reason)
	}
}

func startFastReconnectAgent(t *testing.T, ctx context.Context, addr, dir, name string) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn,
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       dir,
		Name:              name,
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectBackoff:  50 * time.Millisecond,
		// Each refused attempt while revoked doubles the backoff; without a
		// low cap, a loaded run could push the post-unrevoke retry past the
		// test's wait (default cap is 30s).
		MaxReconnectBackoff: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a
}

// The core property: the agent's launcher still holds the *shared*
// pairing token, so merely forgetting the node would let it straight back
// in. A revocation must refuse that identity regardless.
func TestRevokedNodeIsDisconnectedAndRefusedEvenWithSharedToken(t *testing.T) {
	const addr = "127.0.0.1:19500"
	srv := startManager(t, addr, 2*time.Second)
	dir := filepath.Join(t.TempDir(), "revoked-agent")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startFastReconnectAgent(t, ctx, addr, dir, "revoked-agent")
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	events, unsubscribe := srv.Events.Subscribe(32)
	defer unsubscribe()
	revoked, err := srv.RevokeNode(context.Background(), a.NodeID())
	if err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if revoked.NodeID != a.NodeID() || revoked.Name != "revoked-agent" || revoked.RevokedAt.IsZero() {
		t.Fatalf("unexpected revocation record: %+v", revoked)
	}
	if _, ok := srv.Registry.Get(a.NodeID()); ok {
		t.Fatal("expected a revoked node to be removed from the registry immediately")
	}
	sawRevoked := false
	for deadline := time.After(2 * time.Second); !sawRevoked; {
		select {
		case evt := <-events:
			sawRevoked = evt.Type == domain.EventNodeRevoked && evt.NodeID == a.NodeID()
		case <-deadline:
			t.Fatal("expected a node.revoked event")
		}
	}

	// The real agent keeps retrying with its shared token every ~50ms; it
	// must never get back in.
	time.Sleep(500 * time.Millisecond)
	if _, ok := srv.Registry.Get(a.NodeID()); ok {
		t.Fatal("revoked agent re-registered using the shared pairing token")
	}
	expectRevokedReject(t, registerAs(t, addr, dir, pairingToken))

	// Revoking again is idempotent, and revoking an unknown node is an error.
	again, err := srv.RevokeNode(context.Background(), a.NodeID())
	if err != nil || !again.RevokedAt.Equal(revoked.RevokedAt) {
		t.Fatalf("expected idempotent re-revoke returning the original entry, got %+v, %v", again, err)
	}
	if _, err := srv.RevokeNode(context.Background(), "node-never-seen"); err != manager.ErrUnknownNode {
		t.Fatalf("expected ErrUnknownNode, got %v", err)
	}

	// Lifting the revocation lets the same identity back in — through a
	// fresh admission, which its launcher's shared token provides.
	if err := srv.UnrevokeNode(a.NodeID()); err != nil {
		t.Fatalf("UnrevokeNode: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	if err := srv.UnrevokeNode(a.NodeID()); err != manager.ErrNotRevoked {
		t.Fatalf("expected ErrNotRevoked lifting a revocation twice, got %v", err)
	}
}

func TestRevocationSurvivesManagerRestart(t *testing.T) {
	const addr = "127.0.0.1:19501"
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	dir := filepath.Join(t.TempDir(), "restart-revoked")

	store1, err := persistent.Open(dbPath)
	if err != nil {
		t.Fatalf("persistent.Open: %v", err)
	}
	mgrCtx1, mgrCancel1 := context.WithCancel(context.Background())
	srv1 := manager.NewServer(ws.New(), store1, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second})
	go srv1.Run(mgrCtx1)
	if resp := registerAs(t, addr, dir, pairingToken); resp.Type != protocol.MsgRegisterAck {
		t.Fatalf("expected initial admission, got %s", resp.Type)
	}
	id, _ := identity.LoadOrCreate(dir)
	waitFor(t, 3*time.Second, func() bool { _, ok := srv1.Registry.Get(id.NodeID); return ok })
	if _, err := srv1.RevokeNode(context.Background(), id.NodeID); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	mgrCancel1()
	time.Sleep(100 * time.Millisecond)
	store1.Close()

	store2, err := persistent.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	mgrCtx2, mgrCancel2 := context.WithCancel(context.Background())
	defer mgrCancel2()
	srv2 := manager.NewServer(ws.New(), store2, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second})
	go srv2.Run(mgrCtx2)

	expectRevokedReject(t, registerAs(t, addr, dir, pairingToken))
	if _, ok := srv2.Registry.Get(id.NodeID); ok {
		t.Fatal("a revoked node must not be seeded back into the registry after restart")
	}
	if list := srv2.RevokedNodes(); len(list) != 1 || list[0].NodeID != id.NodeID {
		t.Fatalf("expected the revocation to be reloaded, got %+v", list)
	}
}

// A workload pinned to a revoked node can never run anywhere else, so it
// must end CANCELED — not FAILED, which its restart policy would keep
// trying (and deferring) forever.
func TestRevokeCancelsPinnedRestartingWorkload(t *testing.T) {
	const addr = "127.0.0.1:19502"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := manager.NewServer(ws.New(), nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second,
		ReconcileInterval: 50 * time.Millisecond,
	})
	go srv.Run(ctx)
	waitListening(t, addr)
	a := startFastReconnectAgent(t, ctx, addr, filepath.Join(t.TempDir(), "pinned-agent"), "pinned-agent")
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	sleepCmd, sleepArgv := sleepArgs("30")
	wl, err := srv.SubmitWorkload(ctx, a.NodeID(), sleepCmd, sleepArgv, "", nil, domain.ResourceRequirements{}, domain.RestartAlways)
	if err != nil {
		t.Fatalf("SubmitWorkload: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		rec, ok := srv.Workloads.Get(wl.ID)
		return ok && rec.Status.State == domain.WorkloadRunning
	})

	if _, err := srv.RevokeNode(ctx, a.NodeID()); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	rec, _ := srv.Workloads.Get(wl.ID)
	if rec.Status.State != domain.WorkloadCanceled {
		t.Fatalf("expected the pinned workload to be CANCELED by revocation, got %s", rec.Status.State)
	}
	// Several reconcile ticks later it must still be CANCELED with no
	// restart attempt recorded.
	time.Sleep(300 * time.Millisecond)
	rec, _ = srv.Workloads.Get(wl.ID)
	if rec.Status.State != domain.WorkloadCanceled || rec.Restart.Count != 0 {
		t.Fatalf("expected a terminal, never-restarted CANCELED workload, got %s (restarts=%d)", rec.Status.State, rec.Restart.Count)
	}
}

func TestRevocationAPI(t *testing.T) {
	const addr = "127.0.0.1:19503"
	srv := startManager(t, addr, 2*time.Second)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	dir := filepath.Join(t.TempDir(), "api-revoked")
	if resp := registerAs(t, addr, dir, pairingToken); resp.Type != protocol.MsgRegisterAck {
		t.Fatalf("expected admission, got %s", resp.Type)
	}
	id, _ := identity.LoadOrCreate(dir)
	waitFor(t, 3*time.Second, func() bool { _, ok := srv.Registry.Get(id.NodeID); return ok })

	call := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, api.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	if resp := call(http.MethodPost, "/nodes/node-unknown/revoke"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke unknown node: got %d, want 404", resp.StatusCode)
	}
	resp := call(http.MethodPost, "/nodes/"+string(id.NodeID)+"/revoke")
	var revoked domain.RevokedNode
	json.NewDecoder(resp.Body).Decode(&revoked)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || revoked.NodeID != id.NodeID {
		t.Fatalf("revoke: got %d %+v", resp.StatusCode, revoked)
	}

	resp = call(http.MethodGet, "/revocations")
	var list []domain.RevokedNode
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0].NodeID != id.NodeID {
		t.Fatalf("GET /revocations: got %+v", list)
	}
	resp = call(http.MethodGet, "/nodes")
	var nodes []map[string]any
	json.NewDecoder(resp.Body).Decode(&nodes)
	resp.Body.Close()
	if len(nodes) != 0 {
		t.Fatalf("expected a revoked node to disappear from GET /nodes, got %+v", nodes)
	}

	if resp := call(http.MethodDelete, "/revocations/"+string(id.NodeID)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unrevoke: got %d, want 204", resp.StatusCode)
	}
	if resp := call(http.MethodDelete, "/revocations/"+string(id.NodeID)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unrevoke twice: got %d, want 404", resp.StatusCode)
	}
}
