package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/protocol"
)

// sendRawRegister sends one REGISTER with full control over every field a
// hostile peer controls (env.Source, the claimed NodeID, the key that
// signs) and waits for the reply.
func sendRawRegister(t *testing.T, addr string, envSource, claimedID domain.NodeID, pub ed25519.PublicKey, priv ed25519.PrivateKey, token string) {
	t.Helper()
	env, _ := protocol.NewEnvelope(protocol.MsgRegister, envSource, domain.ManagerNodeID, protocol.RegisterPayload{
		Manifest: domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion,
			Node: domain.Node{Identity: domain.Identity{NodeID: claimedID, PublicKey: pub}, Name: "raw"}},
		PairingToken: token, Signature: ed25519.Sign(priv, protocol.RegisterSignedData(token, claimedID)),
	})
	wire, _ := protocol.Encode(env)
	conn := dialWithRetry(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn.Send(ctx, wire)
	conn.Receive(ctx)
	go conn.Close()
}

// A holder of the *shared* pairing token can mint and admit identities at
// will. However many it admits, it must not evict the operator's
// security history (here the in-memory fallback, capped at 500 per log).
func TestSharedTokenAdmissionFloodCannotEvictSecurityLog(t *testing.T) {
	const addr = "127.0.0.1:19513"
	srv := startManager(t, addr, 30*time.Second)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	victimDir := t.TempDir()
	if resp := registerAs(t, addr, victimDir, pairingToken); resp.Type != protocol.MsgRegisterAck {
		t.Fatal("victim admission failed")
	}
	victim := identityFor(t, victimDir).NodeID
	waitFor(t, 3*time.Second, func() bool { _, ok := srv.Registry.Get(victim); return ok })
	resp, err := http.Post(api.URL+"/nodes/"+string(victim)+"/revoke", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()

	for i := 0; i < 600; i++ {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		id := identity.DeriveNodeID(pub)
		sendRawRegister(t, addr, id, id, pub, priv, pairingToken)
	}
	waitFor(t, 5*time.Second, func() bool { return len(srv.Registry.List()) >= 600 })

	security, _ := srv.AuditEntries(domain.AuditSecurity, 0)
	found := false
	for _, e := range security {
		if e.Kind == "node.revoked" && e.NodeID == victim {
			found = true
		}
		if e.Actor == "node" || e.Actor == "anonymous" {
			t.Fatalf("a peer-driven entry reached the security log: %+v", e)
		}
	}
	if !found {
		t.Fatal("600 shared-token admissions evicted the revocation from the security log")
	}
	if admissions, _ := srv.AuditEntries(domain.AuditAdmissions, 0); len(admissions) > 500 {
		t.Fatalf("admissions log exceeded its cap: %d", len(admissions))
	}
}

// The audit log must never record a NodeID the peer merely asserted:
// neither env.Source, nor a claimed ID whose key doesn't match.
func TestRejectionsRecordOnlyVerifiedIdentities(t *testing.T) {
	const addr = "127.0.0.1:19514"
	srv := startManager(t, addr, 30*time.Second)
	victimDir := t.TempDir()
	if resp := registerAs(t, addr, victimDir, pairingToken); resp.Type != protocol.MsgRegisterAck {
		t.Fatal("victim admission failed")
	}
	victim := identityFor(t, victimDir).NodeID

	// 1. A valid key of the attacker's own, wrong token, env.Source naming
	//    the victim: the entry must carry the attacker's verified ID.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	attacker := identity.DeriveNodeID(pub)
	sendRawRegister(t, addr, victim, attacker, pub, priv, "wrong-token")
	// 2. Claiming the victim's NodeID with the attacker's key: unverified,
	//    so no NodeID at all, the claim recorded only as detail.
	sendRawRegister(t, addr, victim, victim, pub, priv, pairingToken)

	// The rejection reply goes out before the audit write, so poll.
	var noise []domain.AuditEntry
	var sawAttacker, sawClaim bool
	deadline := time.Now().Add(3 * time.Second)
	for !(sawAttacker && sawClaim) && time.Now().Before(deadline) {
		noise, _ = srv.AuditEntries(domain.AuditNoise, 0)
		for _, e := range noise {
			if e.Kind != "node.rejected" {
				continue
			}
			if e.NodeID == victim {
				t.Fatalf("the audit log attributed a forged rejection to the victim: %+v", e)
			}
			if e.NodeID == attacker && e.Detail["reason"] == "invalid pairing token" {
				sawAttacker = true
			}
			if e.NodeID == "" && e.Detail["claimedNodeId"] == string(victim) && e.Detail["reason"] == "identity mismatch" {
				sawClaim = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawAttacker || !sawClaim {
		t.Fatalf("expected the attacker's verified rejection and an unverified claim entry, got %+v", noise)
	}
}
