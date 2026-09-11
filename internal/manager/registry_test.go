package manager

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// testNode builds a Manifest declaring CapabilitySystemExecute by default —
// matching real reality post-v4 (every real agent always declares it, see
// sysinfo.Manifest) — so existing placement/reconcile tests that submit a
// default-capability workload against a fixture built from this don't need
// individual updates just because capability enforcement now exists. A test
// specifically about a node lacking a capability builds its own manifest.
func testNode(id domain.NodeID) domain.Manifest {
	return domain.Manifest{
		Node:         domain.Node{Identity: domain.Identity{NodeID: id}, Name: string(id)},
		Capabilities: []domain.Capability{{Name: domain.CapabilitySystemExecute}},
	}
}

// fakeConn is a minimal domain.Conn for identity comparisons in registry
// tests; its methods are never actually invoked.
type fakeConn struct{ tag string }

func (f *fakeConn) Send(context.Context, []byte) error      { return nil }
func (f *fakeConn) Receive(context.Context) ([]byte, error) { return nil, nil }
func (f *fakeConn) RemoteAddr() string                      { return f.tag }
func (f *fakeConn) Close() error                            { return nil }

func TestHasCapability(t *testing.T) {
	rec := &NodeRecord{Capabilities: []domain.Capability{{Name: domain.CapabilitySystemExecute}}}
	if !rec.HasCapability(domain.CapabilitySystemExecute) {
		t.Fatal("expected HasCapability true for a declared capability")
	}
	if rec.HasCapability(domain.CapabilityFilesystemRead) {
		t.Fatal("expected HasCapability false for an undeclared capability")
	}
}

func TestHasCapabilityFalseForNilCapabilities(t *testing.T) {
	rec := &NodeRecord{}
	if rec.HasCapability(domain.CapabilitySystemExecute) {
		t.Fatal("expected HasCapability false when Capabilities is nil")
	}
}

func TestUpsertMarksNewVsKnown(t *testing.T) {
	r := NewRegistry()

	_, isNew := r.Upsert(testNode("node-a"), nil)
	if !isNew {
		t.Fatal("expected first Upsert to report a new node")
	}

	_, isNew = r.Upsert(testNode("node-a"), nil)
	if isNew {
		t.Fatal("expected second Upsert of the same node id to report not-new (reconnect, not duplicate)")
	}

	if got := len(r.List()); got != 1 {
		t.Fatalf("expected exactly 1 node after re-registering the same id, got %d", got)
	}
}

// TestUpsertClearsStaleMetricsOnReconnect proves a reconnecting node
// (agent process restarted, new connection) doesn't keep its previous
// session's live metrics lingering with a non-zero LastHeartbeat — that
// would let a resource-aware placement decision trust arbitrarily old data
// as current until the next real heartbeat arrives.
func TestUpsertClearsStaleMetricsOnReconnect(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("node-a"), &fakeConn{tag: "old"})
	r.RecordHeartbeat("node-a", domain.RuntimeState{MemoryAvailableBytes: 8 << 30, LastHeartbeat: time.Now()})

	rec, _ := r.Get("node-a")
	if rec.LastMetrics.LastHeartbeat.IsZero() {
		t.Fatal("test setup: expected a recorded heartbeat before reconnect")
	}

	r.Upsert(testNode("node-a"), &fakeConn{tag: "new"})

	rec, _ = r.Get("node-a")
	if !rec.LastMetrics.LastHeartbeat.IsZero() {
		t.Fatal("expected reconnect to clear stale LastMetrics from the previous session")
	}
}

func TestExpireStaleTransitionsOnlyTimedOutNodes(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("node-stale"), nil)
	r.Upsert(testNode("node-fresh"), nil)
	r.SetState("node-stale", domain.NodeReady)
	r.SetState("node-fresh", domain.NodeReady)

	// Force node-stale's last-seen into the past without sleeping in the
	// test, by touching node-fresh only after the timeout window opens.
	time.Sleep(30 * time.Millisecond)
	r.Touch("node-fresh")

	expired := r.ExpireStale(20 * time.Millisecond)

	if len(expired) != 1 || expired[0] != "node-stale" {
		t.Fatalf("expected only node-stale to expire, got %v", expired)
	}

	staleRec, _ := r.Get("node-stale")
	if staleRec.State != domain.NodeOffline {
		t.Fatalf("expected node-stale to be OFFLINE, got %s", staleRec.State)
	}

	freshRec, _ := r.Get("node-fresh")
	if freshRec.State != domain.NodeReady {
		t.Fatalf("expected node-fresh to remain READY, got %s", freshRec.State)
	}
}

func TestSetOfflineIfCurrentIgnoresStaleConnection(t *testing.T) {
	r := NewRegistry()
	oldConn := &fakeConn{tag: "old"}
	newConn := &fakeConn{tag: "new"}

	r.Upsert(testNode("node-a"), oldConn)
	r.SetState("node-a", domain.NodeReady)

	// Simulate a fast reconnect: the node re-registers on a new
	// connection before the old connection's own read loop has noticed
	// it is dead.
	r.Upsert(testNode("node-a"), newConn)
	r.SetState("node-a", domain.NodeReady)

	// The stale old connection's cleanup fires late and must not clobber
	// the live reconnect.
	if transitioned := r.SetOfflineIfCurrent("node-a", oldConn); transitioned {
		t.Fatal("expected stale connection's cleanup not to transition a node that has since reconnected on a new connection")
	}
	rec, _ := r.Get("node-a")
	if rec.State != domain.NodeReady {
		t.Fatalf("expected node to remain READY after stale cleanup, got %s", rec.State)
	}

	// The current connection's own cleanup must still work.
	if transitioned := r.SetOfflineIfCurrent("node-a", newConn); !transitioned {
		t.Fatal("expected current connection's cleanup to transition the node offline")
	}
	rec, _ = r.Get("node-a")
	if rec.State != domain.NodeOffline {
		t.Fatalf("expected node to be OFFLINE after current connection's cleanup, got %s", rec.State)
	}
}

func TestExpireStaleIsIdempotentForAlreadyOfflineNodes(t *testing.T) {
	r := NewRegistry()
	r.Upsert(testNode("node-a"), nil)
	r.SetState("node-a", domain.NodeOffline)

	time.Sleep(20 * time.Millisecond)
	expired := r.ExpireStale(10 * time.Millisecond)

	if len(expired) != 0 {
		t.Fatalf("expected already-offline node not to be reported again, got %v", expired)
	}
}
