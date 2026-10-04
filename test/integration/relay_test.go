// Relay integration tests exercise the manager and an agent connected
// entirely through a relay (v5 remote part 1) instead of a direct
// LAN-style dial — the path a device not on the manager's LAN uses. The
// manager is additionally reachable directly (multi.Transport combining
// ws + relay), matching how cmd/manager actually wires the two together,
// so this also proves the two paths coexist without interfering.
package integration

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	relayproto "home-harness/internal/relay"
	"home-harness/internal/transport/multi"
	relaytransport "home-harness/internal/transport/relay"
	"home-harness/internal/transport/ws"
)

const relaySessionToken = "test-relay-session-token"

// startTestRelayServer runs an in-process relay (internal/relay) on
// loopback, standing in for cmd/relay running on a real public address.
func startTestRelayServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := relayproto.NewServer(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return ln.Addr().String()
}

func startManagerWithRelay(t *testing.T, lanAddr, relayAddr string, heartbeatTimeout time.Duration) *manager.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	transport := multi.New().
		Add(ws.New(), lanAddr).
		Add(relaytransport.New(relaySessionToken), relayAddr)

	srv := manager.NewServer(transport, nil, manager.Config{
		Addr:             lanAddr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: heartbeatTimeout,
	})
	go func() {
		if err := srv.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("manager exited: %v", err)
		}
	}()
	waitListening(t, lanAddr)
	return srv
}

// TestAgentRegistersAndHeartbeatsThroughRelay proves an agent that only
// knows a relay address and session token — never the manager's LAN
// address — can register and stay READY, exercising the exact path a
// device not on the manager's LAN uses in production.
func TestAgentRegistersAndHeartbeatsThroughRelay(t *testing.T) {
	const lanAddr = "127.0.0.1:19410"
	relayAddr := startTestRelayServer(t)
	srv := startManagerWithRelay(t, lanAddr, relayAddr, 2*time.Second)

	agentCtx, agentCancel := context.WithCancel(context.Background())
	defer agentCancel()

	a, err := agent.New(relaytransport.NewClient(relaySessionToken), agent.Config{
		ManagerAddr:       relayAddr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "relay-agent-a"),
		Name:              "relay-agent-a",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(agentCtx)

	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	rec, _ := srv.Registry.Get(a.NodeID())
	if rec.Node.Name != "relay-agent-a" {
		t.Fatalf("expected registered node name %q, got %q", "relay-agent-a", rec.Node.Name)
	}
}

// TestCommandRoundTripThroughRelay proves a full command dispatch/result
// cycle — not just registration — works end to end over the relay path,
// the same PING/ECHO round trip commands_test.go proves for the direct
// LAN path.
func TestCommandRoundTripThroughRelay(t *testing.T) {
	const lanAddr = "127.0.0.1:19411"
	relayAddr := startTestRelayServer(t)
	srv := startManagerWithRelay(t, lanAddr, relayAddr, 2*time.Second)

	agentCtx, agentCancel := context.WithCancel(context.Background())
	defer agentCancel()

	a, err := agent.New(relaytransport.NewClient(relaySessionToken), agent.Config{
		ManagerAddr:       relayAddr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "relay-agent-b"),
		Name:              "relay-agent-b",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(agentCtx)

	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := srv.SendCommand(ctx, a.NodeID(), domain.CommandPing, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("SendCommand(PING) over relay: %v", err)
	}
	if !result.Success || result.Output["message"] != "pong" {
		t.Fatalf("expected successful pong over relay, got %+v", result)
	}
}

// TestAgentReconnectsThroughRelayAfterDrop proves the manager's relay
// listener pool (internal/transport/relay's pendingPoolSize) keeps enough
// spare registrations parked that an agent's own exponential-backoff
// reconnect loop (internal/agent's Run) succeeds promptly after a drop,
// without the two racing each other — the scenario a real network blip
// produces, and the reason the pool holds more than one slot at a time.
func TestAgentReconnectsThroughRelayAfterDrop(t *testing.T) {
	const lanAddr = "127.0.0.1:19412"
	relayAddr := startTestRelayServer(t)
	srv := startManagerWithRelay(t, lanAddr, relayAddr, 2*time.Second)

	identityDir := filepath.Join(t.TempDir(), "relay-agent-c")
	agentCtx, agentCancel := context.WithCancel(context.Background())

	a, err := agent.New(relaytransport.NewClient(relaySessionToken), agent.Config{
		ManagerAddr:       relayAddr,
		PairingToken:      pairingToken,
		IdentityDir:       identityDir,
		Name:              "relay-agent-c",
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectBackoff:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(agentCtx)

	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	originalNodeID := a.NodeID()

	// Simulate a dropped connection (WiFi blip, not a clean shutdown) by
	// killing the agent's context without a graceful close, exactly as
	// TestOfflineDetectionAndReconnectSameNodeID does for the LAN path.
	agentCancel()
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(originalNodeID)
		return ok && rec.State == domain.NodeOffline
	})

	agentCtx2, agentCancel2 := context.WithCancel(context.Background())
	defer agentCancel2()
	a2, err := agent.New(relaytransport.NewClient(relaySessionToken), agent.Config{
		ManagerAddr:       relayAddr,
		PairingToken:      pairingToken,
		IdentityDir:       identityDir,
		Name:              "relay-agent-c",
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectBackoff:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New (reconnect): %v", err)
	}
	if a2.NodeID() != originalNodeID {
		t.Fatalf("expected same node id on reconnect, got %q want %q", a2.NodeID(), originalNodeID)
	}
	go a2.Run(agentCtx2)

	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(originalNodeID)
		return ok && rec.State == domain.NodeReady
	})
}
