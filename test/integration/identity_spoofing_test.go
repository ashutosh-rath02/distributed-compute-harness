package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

// rawRegister dials the manager directly (bypassing internal/agent) so a
// test can send a deliberately malformed or forged REGISTER, and returns
// whatever envelope the manager sends back.
func rawRegister(t *testing.T, addr string, payload protocol.RegisterPayload, claimedID domain.NodeID) *protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := ws.New().Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	env, err := protocol.NewEnvelope(protocol.MsgRegister, claimedID, domain.ManagerNodeID, payload)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	wire, err := protocol.Encode(env)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatalf("Send: %v", err)
	}

	data, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	respEnv, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("Decode response: %v", err)
	}
	return respEnv
}

func TestRegisterRejectsNodeIDNotDerivedFromPublicKey(t *testing.T) {
	const addr = "127.0.0.1:19194"
	srv := startManager(t, addr, 2*time.Second)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	forgedID := domain.NodeID("node-not-derived-from-key")

	sig := ed25519.Sign(priv, protocol.RegisterSignedData(pairingToken, forgedID))
	payload := protocol.RegisterPayload{
		Manifest: domain.Manifest{
			SchemaVersion: domain.ManifestSchemaVersion,
			Node: domain.Node{
				Identity: domain.Identity{NodeID: forgedID, PublicKey: []byte(pub)},
				Name:     "attacker",
			},
		},
		PairingToken: pairingToken,
		Signature:    sig,
	}

	resp := rawRegister(t, addr, payload, forgedID)
	if resp.Type != protocol.MsgRegisterReject {
		t.Fatalf("expected REGISTER_REJECT for a NodeID not derived from the public key, got %s", resp.Type)
	}
	if _, ok := srv.Registry.Get(forgedID); ok {
		t.Fatal("forged node must not appear in the registry")
	}
}

func TestRegisterRejectsInvalidSignature(t *testing.T) {
	const addr = "127.0.0.1:19195"
	srv := startManager(t, addr, 2*time.Second)

	// A real agent's identity, so NodeID correctly matches the public key.
	victim, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "victim"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	payload := protocol.RegisterPayload{
		Manifest: domain.Manifest{
			SchemaVersion: domain.ManifestSchemaVersion,
			Node: domain.Node{
				Identity: victim.Identity,
				Name:     "impersonator",
			},
		},
		PairingToken: pairingToken,
		// Attacker knows the pairing token and the victim's public
		// identity (both are not secret) but does not hold the private
		// key, so they cannot produce a valid signature.
		Signature: []byte("not-a-real-signature"),
	}

	resp := rawRegister(t, addr, payload, victim.NodeID)
	if resp.Type != protocol.MsgRegisterReject {
		t.Fatalf("expected REGISTER_REJECT for an unsigned/forged registration, got %s", resp.Type)
	}
	if _, ok := srv.Registry.Get(victim.NodeID); ok {
		t.Fatal("impersonated node must not appear in the registry without a valid signature")
	}
}

func TestHeartbeatCannotSpoofAnotherNodesSource(t *testing.T) {
	const addr = "127.0.0.1:19196"
	srv := startManager(t, addr, 2*time.Second)

	agentCtx, agentCancel := context.WithCancel(context.Background())
	defer agentCancel()

	victimAgent, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "victim-agent"),
		Name:              "victim-agent",
		HeartbeatInterval: 5 * time.Second, // slow, so we can tell if it's the only source of heartbeats
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go victimAgent.Run(agentCtx)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(victimAgent.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	rec, _ := srv.Registry.Get(victimAgent.NodeID())
	lastSeenBeforeAttack := rec.LastSeen

	// An unregistered connection tries to send a HEARTBEAT claiming to be
	// the victim's NodeID, without ever completing its own REGISTER.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := ws.New().Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	forged, err := protocol.NewEnvelope(protocol.MsgHeartbeat, victimAgent.NodeID(), domain.ManagerNodeID,
		protocol.HeartbeatPayload{RuntimeState: domain.RuntimeState{NodeID: victimAgent.NodeID(), State: domain.NodeReady}})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	wire, err := protocol.Encode(forged)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatalf("Send: %v", err)
	}

	data, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	resp, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if resp.Type != protocol.MsgError {
		t.Fatalf("expected an ERROR for a HEARTBEAT on an unregistered connection, got %s", resp.Type)
	}

	rec, _ = srv.Registry.Get(victimAgent.NodeID())
	if !rec.LastSeen.Equal(lastSeenBeforeAttack) {
		t.Fatal("victim node's LastSeen must not be updated by a forged heartbeat from an unregistered connection")
	}
}
