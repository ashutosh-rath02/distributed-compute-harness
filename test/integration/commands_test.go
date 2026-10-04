package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/transport/ws"
)

func startRegisteredAgent(t *testing.T, addr, name string) *agent.Agent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn,
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), name),
		Name:              name,
		HeartbeatInterval: 100 * time.Millisecond,
		// The manager binds its listener asynchronously, so a first dial
		// can lose that race under load; with the 3s default backoff the
		// retry would land at callers' 3s waits. Retry fast instead.
		ReconnectBackoff:    50 * time.Millisecond,
		MaxReconnectBackoff: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a
}

func TestCommandPingEcho(t *testing.T) {
	const addr = "127.0.0.1:19210"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "cmd-agent-a")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := srv.SendCommand(ctx, a.NodeID(), domain.CommandPing, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("SendCommand(PING): %v", err)
	}
	if !result.Success || result.Output["message"] != "pong" {
		t.Fatalf("expected successful pong, got %+v", result)
	}

	result, err = srv.SendCommand(ctx, a.NodeID(), domain.CommandEcho, map[string]string{"message": "hello harness"}, 2*time.Second)
	if err != nil {
		t.Fatalf("SendCommand(ECHO): %v", err)
	}
	if !result.Success || result.Output["message"] != "hello harness" {
		t.Fatalf("expected echoed message, got %+v", result)
	}
}

func TestCommandGetSystemInfoAndAgentStatus(t *testing.T) {
	const addr = "127.0.0.1:19211"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "cmd-agent-b")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	info, err := srv.SendCommand(ctx, a.NodeID(), domain.CommandGetSystemInfo, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("SendCommand(GET_SYSTEM_INFO): %v", err)
	}
	if !info.Success || info.Output["os"] == "" {
		t.Fatalf("expected system info with a non-empty OS, got %+v", info)
	}

	status, err := srv.SendCommand(ctx, a.NodeID(), domain.CommandGetAgentStatus, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("SendCommand(GET_AGENT_STATUS): %v", err)
	}
	if !status.Success || status.Output["nodeId"] != string(a.NodeID()) {
		t.Fatalf("expected agent status naming its own node id, got %+v", status)
	}
}

func TestCommandRequestResourceRefreshUpdatesRegistry(t *testing.T) {
	const addr = "127.0.0.1:19212"
	srv := startManager(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "cmd-agent-c")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	// Registration already populates resources (Milestone 6), so clear
	// them first to prove REQUEST_RESOURCE_REFRESH is what repopulates
	// them, not leftover state from REGISTER.
	srv.Registry.UpdateResources(a.NodeID(), nil, nil)
	rec, _ := srv.Registry.Get(a.NodeID())
	if len(rec.Resources) != 0 {
		t.Fatal("test setup: expected resources to be cleared")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := srv.SendCommand(ctx, a.NodeID(), domain.CommandRequestResourceRefresh, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("SendCommand(REQUEST_RESOURCE_REFRESH): %v", err)
	}
	if !result.Success {
		t.Fatalf("expected successful refresh, got %+v", result)
	}

	waitFor(t, 2*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && len(rec.Resources) > 0
	})
}

func TestSendCommandToUnknownNodeFails(t *testing.T) {
	const addr = "127.0.0.1:19213"
	srv := startManager(t, addr, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := srv.SendCommand(ctx, "node-does-not-exist", domain.CommandPing, nil, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected SendCommand to fail for an unknown node")
	}
}
