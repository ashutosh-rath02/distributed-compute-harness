package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

func startAgentWithFingerprint(t *testing.T, ctx context.Context, addr, name, fingerprint string) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond,
		MaxReconnectBackoff: 200 * time.Millisecond, HostFingerprint: fingerprint,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a
}

type fleetNodeView struct {
	NodeID       domain.NodeID     `json:"nodeId"`
	Alias        string            `json:"alias"`
	Labels       map[string]string `json:"labels"`
	SameHostAs   []domain.NodeID   `json:"sameHostAs"`
	HostConflict bool              `json:"hostConflict"`
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// Two real agents reporting the same machine (the injected fingerprint
// stands in for two identity dirs on one PC) are flagged, and the
// machine's capacity counts once; a third on a "different machine" is
// unaffected.
func TestSameMachineIdentitiesAreFlaggedAndCountedOnce(t *testing.T) {
	const addr = "127.0.0.1:19508"
	srv := startManager(t, addr, 2*time.Second)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startAgentWithFingerprint(t, ctx, addr, "desk-identity-1", "fp-shared-desk")
	b := startAgentWithFingerprint(t, ctx, addr, "desk-identity-2", "fp-shared-desk")
	c := startAgentWithFingerprint(t, ctx, addr, "other-machine", "fp-other")
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range []domain.NodeID{a.NodeID(), b.NodeID(), c.NodeID()} {
			if rec, ok := srv.Registry.Get(id); !ok || rec.State != domain.NodeReady || len(rec.Resources) == 0 {
				return false
			}
		}
		return true
	})

	var nodes []fleetNodeView
	getJSON(t, api.URL+"/nodes", &nodes)
	byID := map[domain.NodeID]fleetNodeView{}
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	if v := byID[a.NodeID()]; len(v.SameHostAs) != 1 || v.SameHostAs[0] != b.NodeID() || v.HostConflict {
		t.Fatalf("identity 1 should report the same machine as identity 2: %+v", v)
	}
	if v := byID[c.NodeID()]; len(v.SameHostAs) != 0 {
		t.Fatalf("the other machine must not be grouped: %+v", v)
	}

	rec, _ := srv.Registry.Get(c.NodeID())
	var perMachineCores float64
	for _, r := range rec.Resources {
		if r.Kind == domain.ResourceCPUCores {
			perMachineCores = r.Capacity
		}
	}
	var totals map[string]float64
	getJSON(t, api.URL+"/resources/total", &totals)
	// All three run on this PC with identical hardware; the two sharing a
	// fingerprint count once, so the total is two machines' worth.
	if got := totals[string(domain.ResourceCPUCores)]; got != 2*perMachineCores {
		t.Fatalf("expected %v cores (shared desk counted once + other machine), got %v", 2*perMachineCores, got)
	}
}

// A fingerprint is agent-asserted: a match without matching hardware is a
// conflict, and both identities' capacity still counts.
func TestFingerprintMatchWithDifferentHardwareIsAConflict(t *testing.T) {
	const addr = "127.0.0.1:19509"
	srv := startManager(t, addr, 2*time.Second)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	real := startAgentWithFingerprint(t, ctx, addr, "real-machine", "fp-copied")
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(real.NodeID())
		return ok && len(rec.Resources) > 0
	})
	// A raw node claiming the same fingerprint with different hardware.
	impostor, conn := registerRawNodeWithManifest(t, addr, "impostor", func(m *domain.Manifest) {
		m.Node.HostFingerprint = "fp-copied"
		m.Resources = []domain.Resource{{Kind: domain.ResourceCPUCores, Capacity: 1}, {Kind: domain.ResourceMemoryBytes, Capacity: 1}}
	})
	defer conn.Close()

	var nodes []fleetNodeView
	getJSON(t, api.URL+"/nodes", &nodes)
	for _, n := range nodes {
		if (n.NodeID == real.NodeID() || n.NodeID == impostor) && !n.HostConflict {
			t.Fatalf("expected a host conflict, got %+v", n)
		}
	}
	realRec, _ := srv.Registry.Get(real.NodeID())
	var realCores float64
	for _, r := range realRec.Resources {
		if r.Kind == domain.ResourceCPUCores {
			realCores = r.Capacity
		}
	}
	var totals map[string]float64
	getJSON(t, api.URL+"/resources/total", &totals)
	if got := totals[string(domain.ResourceCPUCores)]; got != realCores+1 {
		t.Fatalf("a conflict must count both identities (%v+1), got %v", realCores, got)
	}
}

// registerRawNodeWithManifest registers a raw (test-controlled) node,
// letting the test shape its manifest.
func registerRawNodeWithManifest(t *testing.T, addr, name string, shape func(*domain.Manifest)) (domain.NodeID, interface{ Close() error }) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	id := identityFor(t, dir)
	manifest := domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Node: domain.Node{Identity: id.Identity, Name: name}}
	shape(&manifest)
	conn := dialWithRetry(t, addr)
	env, _ := protocol.NewEnvelope(protocol.MsgRegister, id.NodeID, domain.ManagerNodeID, protocol.RegisterPayload{
		Manifest: manifest, PairingToken: pairingToken, Signature: id.Sign(protocol.RegisterSignedData(pairingToken, id.NodeID)),
	})
	wire, _ := protocol.Encode(env)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatalf("send REGISTER: %v", err)
	}
	if _, err := conn.Receive(ctx); err != nil {
		t.Fatalf("receive REGISTER reply: %v", err)
	}
	return id.NodeID, conn
}

func TestAliasAndLabelsPersistAndRevocationClearsThem(t *testing.T) {
	const addr = "127.0.0.1:19510"
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	identityDir := filepath.Join(t.TempDir(), "labelled")
	start := func() (*manager.Server, *persistent.Store, context.CancelFunc) {
		store, err := persistent.Open(dbPath)
		if err != nil {
			t.Fatalf("persistent.Open: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		srv := manager.NewServer(ws.New(), store, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second})
		go srv.Run(ctx)
		return srv, store, cancel
	}
	srv, store, stop := start()
	resp := registerAs(t, addr, identityDir, pairingToken)
	if resp.Type != protocol.MsgRegisterAck {
		t.Fatalf("admission: %s", resp.Type)
	}
	id := identityFor(t, identityDir).NodeID
	waitFor(t, 3*time.Second, func() bool { _, ok := srv.Registry.Get(id); return ok })
	if _, err := srv.SetNodeMeta(id, domain.NodeMeta{Alias: "Living-room PC", Labels: map[string]string{"gpu": "iris-xe"}}); err != nil {
		t.Fatalf("SetNodeMeta: %v", err)
	}
	stop()
	time.Sleep(100 * time.Millisecond)
	store.Close()

	srv, store, stop = start()
	defer func() { stop(); time.Sleep(50 * time.Millisecond); store.Close() }()
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	waitFor(t, 3*time.Second, func() bool { _, ok := srv.Registry.Get(id); return ok })
	// The node reconnects (re-sending its own manifest name); the
	// operator's alias must survive both the restart and the reconnect.
	if resp := registerAs(t, addr, identityDir, pairingToken); resp.Type != protocol.MsgRegisterAck {
		t.Fatalf("reconnect: %s", resp.Type)
	}
	var view fleetNodeView
	getJSON(t, api.URL+"/nodes/"+string(id), &view)
	if view.Alias != "Living-room PC" || view.Labels["gpu"] != "iris-xe" {
		t.Fatalf("metadata lost across restart/reconnect: %+v", view)
	}
	if _, err := srv.RevokeNode(context.Background(), id); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if metas, _ := store.ListNodeMeta(); len(metas) != 0 {
		t.Fatalf("revocation must clear the node's metadata, got %+v", metas)
	}
}

// The audit trail over real admissions: how each node got in, routine
// reconnects in the noise log, and a revoked agent's retry loop producing
// one rejection entry rather than one per attempt.
func TestAuditTrailOfAdmissionsAndRejections(t *testing.T) {
	const addr = "127.0.0.1:19511"
	srv := startManager(t, addr, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startAgentWithFingerprint(t, ctx, addr, "audited", "-")
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	if _, err := srv.RevokeNode(ctx, a.NodeID()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // the revoked agent retries every ~200ms

	admissions, _ := srv.AuditEntries(domain.AuditAdmissions, 0)
	admitted := false
	for _, e := range admissions {
		if e.Kind == "node.admitted" && e.NodeID == a.NodeID() && e.Detail["by"] == "pairing-token" {
			admitted = true
		}
	}
	if !admitted {
		t.Fatalf("expected node.admitted by pairing-token in the admissions log, got %+v", admissions)
	}
	noise, _ := srv.AuditEntries(domain.AuditNoise, 0)
	rejected := 0
	for _, e := range noise {
		if e.Kind == "node.rejected" && e.NodeID == a.NodeID() {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("expected exactly one rejection entry for the retrying revoked agent, got %d (%+v)", rejected, noise)
	}
}

// The plan's flood test, against the real REGISTER path: thousands of
// fresh identities with a wrong token must not evict a revocation from
// the security log, must stay within the per-minute ceiling, and must not
// grow the limiter beyond its bound.
func TestRegistrationFloodCannotEvictSecurityHistory(t *testing.T) {
	const addr = "127.0.0.1:19512"
	srv := startManager(t, addr, 2*time.Second)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	victimDir := filepath.Join(t.TempDir(), "victim")
	if resp := registerAs(t, addr, victimDir, pairingToken); resp.Type != protocol.MsgRegisterAck {
		t.Fatal("victim admission failed")
	}
	victim := identityFor(t, victimDir).NodeID
	waitFor(t, 3*time.Second, func() bool { _, ok := srv.Registry.Get(victim); return ok })
	resp, err := http.Post(api.URL+"/nodes/"+string(victim)+"/revoke", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke via API: %v", err)
	}
	resp.Body.Close()

	for i := 0; i < 400; i++ { // well past the 30/minute ceiling
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		nodeID := identity.DeriveNodeID(pub)
		env, _ := protocol.NewEnvelope(protocol.MsgRegister, nodeID, domain.ManagerNodeID, protocol.RegisterPayload{
			Manifest: domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion,
				Node: domain.Node{Identity: domain.Identity{NodeID: nodeID, PublicKey: pub}}},
			PairingToken: "wrong", Signature: ed25519.Sign(priv, protocol.RegisterSignedData("wrong", nodeID)),
		})
		wire, _ := protocol.Encode(env)
		c := dialWithRetry(t, addr)
		sendCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c.Send(sendCtx, wire)
		c.Receive(sendCtx)
		cancel()
		go c.Close()
	}

	var security []domain.AuditEntry
	getJSON(t, api.URL+"/audit?log=security&limit=1000", &security)
	found := false
	for _, e := range security {
		if e.Kind == "node.revoked" && e.NodeID == victim {
			found = true
		}
	}
	if !found {
		t.Fatal("the flood evicted the revocation from the security log")
	}
	var noise []domain.AuditEntry
	getJSON(t, api.URL+"/audit?log=noise&limit=1000", &noise)
	rejections := 0
	for _, e := range noise {
		if e.Kind == "node.rejected" {
			rejections++
		}
	}
	if rejections > 90 { // 30/min; the flood spans at most a couple of minutes
		t.Fatalf("rejection ceiling not enforced: %d entries", rejections)
	}
}

func identityFor(t *testing.T, dir string) *identity.Identity {
	t.Helper()
	id, err := identity.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("identity.LoadOrCreate: %v", err)
	}
	return id
}
