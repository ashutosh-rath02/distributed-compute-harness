package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/protocol"
)

// registerRawSilentNode completes a real REGISTER handshake over a raw
// connection that the test fully controls, then returns without starting
// any heartbeat/receive loop — the "node" never replies to anything sent
// to it afterward. This gives deterministic control over exactly when the
// connection dies relative to an in-flight command, which a real agent
// (replying in well under a millisecond on localhost) does not.
func registerRawSilentNode(t *testing.T, addr, name string) (domain.NodeID, domain.Conn) {
	t.Helper()
	id, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("identity.LoadOrCreate: %v", err)
	}

	conn := dialWithRetry(t, addr)

	manifest := domain.Manifest{
		SchemaVersion: domain.ManifestSchemaVersion,
		Node:          domain.Node{Identity: id.Identity, Name: name},
		// Declares system.execute like a real agent's sysinfo.Manifest does
		// post-v4, so placement against this raw/silent node (used to test
		// connection/heartbeat behavior, not capability enforcement) works
		// the same as it did before capability checking existed.
		Capabilities: []domain.Capability{{Name: domain.CapabilitySystemExecute}},
	}
	signature := id.Sign(protocol.RegisterSignedData(pairingToken, id.NodeID))
	env, err := protocol.NewEnvelope(protocol.MsgRegister, id.NodeID, domain.ManagerNodeID,
		protocol.RegisterPayload{Manifest: manifest, PairingToken: pairingToken, Signature: signature})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	wire, err := protocol.Encode(env)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatalf("send REGISTER: %v", err)
	}

	data, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("receive REGISTER_ACK: %v", err)
	}
	ackEnv, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode REGISTER_ACK: %v", err)
	}
	if ackEnv.Type != protocol.MsgRegisterAck {
		t.Fatalf("expected REGISTER_ACK, got %s", ackEnv.Type)
	}

	return id.NodeID, conn
}

// TestSendCommandFailsPromptlyOnDisconnect proves the manager doesn't
// strand a caller for the full command timeout when the target node's
// connection dies mid-command — SendCommand should resolve quickly with a
// failure once the connection closes, not silently wait out the deadline.
func TestSendCommandFailsPromptlyOnDisconnect(t *testing.T) {
	const addr = "127.0.0.1:19240"
	// Long heartbeat timeout: this test disconnects by closing the raw
	// connection directly, not by letting heartbeats lapse.
	srv := startManager(t, addr, 30*time.Second)

	nodeID, conn := registerRawSilentNode(t, addr, "flaky-agent")
	defer conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultCh := make(chan domain.CommandResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := srv.SendCommand(ctx, nodeID, domain.CommandPing, nil, 20*time.Second)
		resultCh <- result
		errCh <- err
	}()

	// The raw node never reads or replies to the COMMAND — give the
	// manager a brief moment to have sent it and registered it as
	// pending, then kill the connection out from under it.
	time.Sleep(50 * time.Millisecond)
	conn.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected SendCommand to return a failed result (not an error), got err: %v", err)
		}
		result := <-resultCh
		if result.Success {
			t.Fatalf("expected a failed command result after disconnect, got %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendCommand did not return promptly after the node's connection closed — it appears to be stuck waiting for the full timeout")
	}
}
