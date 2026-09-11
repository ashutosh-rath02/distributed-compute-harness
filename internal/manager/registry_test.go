package manager

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func testNode(id domain.NodeID) domain.Manifest {
	return domain.Manifest{
		Node: domain.Node{Identity: domain.Identity{NodeID: id}, Name: string(id)},
	}
}

// fakeConn is a minimal domain.Conn for identity comparisons in registry
// tests; its methods are never actually invoked.
type fakeConn struct{ tag string }

func (f *fakeConn) Send(context.Context, []byte) error      { return nil }
func (f *fakeConn) Receive(context.Context) ([]byte, error) { return nil, nil }
func (f *fakeConn) RemoteAddr() string                      { return f.tag }
func (f *fakeConn) Close() error                            { return nil }

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
