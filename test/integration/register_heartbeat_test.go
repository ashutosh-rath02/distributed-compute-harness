// Package integration exercises the manager and agent together over a
// real WebSocket transport on localhost, validating the flows a naive
// implementation gets wrong: register/heartbeat end-to-end, heartbeat-
// expiry offline detection, and reconnect with the same node ID.
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
	"home-harness/internal/transport/ws"
)

const pairingToken = "test-pairing-token"

func startManager(t *testing.T, addr string, heartbeatTimeout time.Duration) *manager.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := manager.NewServer(ws.New(), nil, manager.Config{
		Addr:             addr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: heartbeatTimeout,
	})
	go func() {
		if err := srv.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("manager exited: %v", err)
		}
	}()
	waitListening(t, addr)
	return srv
}

// waitListening returns once something accepts TCP connections at addr:
// the manager binds its port in its own goroutine (Run), and an agent
// that dials first waits out its whole reconnect backoff (3 s by
// default) — longer than many tests wait for READY on a loaded machine.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager not listening on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dialWithRetry dials addr, retrying briefly to tolerate the manager's
// Listen() binding the port asynchronously in its own goroutine (started
// via startManager) rather than before returning.
func dialWithRetry(t *testing.T, addr string) domain.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		dialCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		conn, err := ws.New().Dial(dialCtx, addr)
		cancel()
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dial %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition not met before timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRegisterAndHeartbeat(t *testing.T) {
	const addr = "127.0.0.1:19191"
	srv := startManager(t, addr, 500*time.Millisecond)

	agentCtx, agentCancel := context.WithCancel(context.Background())
	defer agentCancel()

	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "agent-a"),
		Name:              "test-agent-a",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(agentCtx)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	rec, _ := srv.Registry.Get(a.NodeID())
	if rec.Node.Name != "test-agent-a" {
		t.Fatalf("expected registered node name %q, got %q", "test-agent-a", rec.Node.Name)
	}

	firstSeen := rec.LastSeen
	waitFor(t, 2*time.Second, func() bool {
		rec, _ := srv.Registry.Get(a.NodeID())
		return rec.LastSeen.After(firstSeen)
	})
}

func TestOfflineDetectionAndReconnectSameNodeID(t *testing.T) {
	const addr = "127.0.0.1:19192"
	srv := startManager(t, addr, 300*time.Millisecond)

	identityDir := filepath.Join(t.TempDir(), "agent-b")

	agentCtx, agentCancel := context.WithCancel(context.Background())
	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       identityDir,
		Name:              "test-agent-b",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(agentCtx)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	originalNodeID := a.NodeID()

	// Simulate the laptop losing Wi-Fi: kill the agent's connection
	// without a clean close, and confirm the manager notices via
	// heartbeat expiry rather than an immediate disconnect signal.
	agentCancel()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(originalNodeID)
		return ok && rec.State == domain.NodeOffline
	})

	// Reconnect: a fresh agent process loading the SAME identity dir must
	// resolve to the same NodeID and must not create a duplicate node.
	agentCtx2, agentCancel2 := context.WithCancel(context.Background())
	defer agentCancel2()
	a2, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       identityDir,
		Name:              "test-agent-b",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New (reconnect): %v", err)
	}
	if a2.NodeID() != originalNodeID {
		t.Fatalf("expected same node id on reconnect, got %q want %q", a2.NodeID(), originalNodeID)
	}
	go a2.Run(agentCtx2)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(originalNodeID)
		return ok && rec.State == domain.NodeReady
	})

	if got := len(srv.Registry.List()); got != 1 {
		t.Fatalf("expected exactly 1 node in registry after reconnect, got %d", got)
	}
}

func TestRegistrationRejectedWithBadPairingToken(t *testing.T) {
	const addr = "127.0.0.1:19193"
	srv := startManager(t, addr, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      "wrong-token",
		IdentityDir:       filepath.Join(t.TempDir(), "agent-c"),
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectBackoff:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)

	// Give it a couple of reconnect/reject cycles, then confirm it never
	// gets admitted into the registry.
	time.Sleep(300 * time.Millisecond)
	if _, ok := srv.Registry.Get(a.NodeID()); ok {
		t.Fatal("expected node with bad pairing token to never be registered")
	}
}
