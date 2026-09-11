// TestV1DemoScenario drives the exact demo from v1.md §18 end to end
// against real manager/agent code (WS transport, real identities, real
// sysinfo) rather than testing components in isolation. It is the
// project's acceptance test: if this passes, v0 has succeeded per v1.md's
// own definition. Step numbers in comments match v1.md §18 (the manager
// itself isn't a domain.Node in this architecture — no agent code runs on
// it — so where v1.md's narrative counts it as one of "3 nodes," this test
// counts 2 *agents* instead, which is the architecturally accurate
// reading of the same scenario).
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

func expectEvent(t *testing.T, events <-chan domain.Event, wantType domain.EventType, wantNode domain.NodeID, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-events:
			if e.Type == wantType && e.NodeID == wantNode {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %s for node %s", wantType, wantNode)
		}
	}
}

func TestV1DemoScenario(t *testing.T) {
	const addr = "127.0.0.1:19230"

	// Step 1: Start Manager on Computer A.
	srv := startManager(t, addr, 700*time.Millisecond)

	events, unsubscribe := srv.Events.Subscribe(64)
	defer unsubscribe()

	// Step 2-3: Start Agent on Computer B. Agent discovers Manager (via
	// the manual-address fallback here — multicast discovery is covered
	// separately, see discovery_test.go). Node registers.
	identityDirB := filepath.Join(t.TempDir(), "laptop-b")
	ctxB, cancelB := context.WithCancel(context.Background())
	agentB, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       identityDirB,
		Name:              "Laptop-B",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New(Laptop-B): %v", err)
	}
	go agentB.Run(ctxB)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(agentB.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	expectEvent(t, events, domain.EventNodeRegistered, agentB.NodeID(), 2*time.Second)

	// Step 4: Start another Agent on Computer C. Manager now shows
	// multiple nodes.
	agentC := startRegisteredAgent(t, addr, "Desktop-C")
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(agentC.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	if got := len(srv.Registry.List()); got != 2 {
		t.Fatalf("expected 2 known nodes after both agents register, got %d", got)
	}

	// Step 5: Each device reports resources. Total visible resources is
	// informational only — no pooled execution (v1.md §18 step 5).
	waitFor(t, 2*time.Second, func() bool {
		return srv.Registry.TotalResources()[domain.ResourceCPUCores] > 0
	})
	totals := srv.Registry.TotalResources()
	if totals[domain.ResourceCPUCores] <= 0 || totals[domain.ResourceMemoryBytes] <= 0 || totals[domain.ResourceStorageBytes] <= 0 {
		t.Fatalf("expected positive totals for cpu/memory/storage, got %+v", totals)
	}

	// Step 6: Manager sends PING to Laptop-B. Laptop responds PONG.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pingResult, err := srv.SendCommand(ctx, agentB.NodeID(), domain.CommandPing, nil, 2*time.Second)
	if err != nil || !pingResult.Success || pingResult.Output["message"] != "pong" {
		t.Fatalf("expected PING -> pong, got result=%+v err=%v", pingResult, err)
	}

	// Step 7: Manager sends GET_SYSTEM_INFO. Laptop replies.
	infoResult, err := srv.SendCommand(ctx, agentB.NodeID(), domain.CommandGetSystemInfo, nil, 2*time.Second)
	if err != nil || !infoResult.Success || infoResult.Output["os"] == "" {
		t.Fatalf("expected GET_SYSTEM_INFO to succeed with a populated OS field, got result=%+v err=%v", infoResult, err)
	}

	// Step 8: Disconnect Laptop-B. Manager detects OFFLINE and emits
	// node.offline. (The dedicated heartbeat-timeout mechanism is unit
	// tested deterministically in internal/manager/registry_test.go; this
	// step confirms the end-to-end disconnect path the demo describes.)
	cancelB()
	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(agentB.NodeID())
		return ok && rec.State == domain.NodeOffline
	})
	expectEvent(t, events, domain.EventNodeOffline, agentB.NodeID(), 2*time.Second)

	// Step 9: Reconnect the laptop (same identity dir -> same NodeID).
	// Same Node ID returns RECONNECTED. No duplicate node is created.
	ctxB2, cancelB2 := context.WithCancel(context.Background())
	defer cancelB2()
	agentB2, err := agent.New(ws.New(), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       identityDirB,
		Name:              "Laptop-B",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New(Laptop-B reconnect): %v", err)
	}
	if agentB2.NodeID() != agentB.NodeID() {
		t.Fatalf("expected reconnect to use the same node id, got %q want %q", agentB2.NodeID(), agentB.NodeID())
	}
	go agentB2.Run(ctxB2)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(agentB.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	expectEvent(t, events, domain.EventNodeReconnected, agentB.NodeID(), 2*time.Second)

	if got := len(srv.Registry.List()); got != 2 {
		t.Fatalf("expected still exactly 2 nodes after reconnect (no duplicate), got %d", got)
	}
}
