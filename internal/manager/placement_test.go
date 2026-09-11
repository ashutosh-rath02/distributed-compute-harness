package manager

import (
	"errors"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// nodeWithProfile builds a NodeRecord with a fabricated static/live
// resource profile, for deterministic placement tests independent of real
// sysinfo or a real Registry.
func nodeWithProfile(id domain.NodeID, cpuCores float64, memAvailable uint64, cpuPercent float64, heartbeated bool) *NodeRecord {
	rec := &NodeRecord{
		Node:      domain.Node{Identity: domain.Identity{NodeID: id}, Name: string(id)},
		Resources: []domain.Resource{{Kind: domain.ResourceCPUCores, Capacity: cpuCores, Unit: "cores"}},
		State:     domain.NodeReady,
	}
	if heartbeated {
		rec.LastMetrics = domain.RuntimeState{
			MemoryAvailableBytes: memAvailable,
			CPUPercent:           cpuPercent,
			LastHeartbeat:        time.Now(),
		}
	}
	return rec
}

func TestSelectNodePicksMostAvailableMemory(t *testing.T) {
	low := nodeWithProfile("node-low", 4, 1<<30, 10, true)   // 1 GiB
	high := nodeWithProfile("node-high", 4, 8<<30, 10, true) // 8 GiB

	got, err := selectNode([]*NodeRecord{low, high}, domain.ResourceRequirements{})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	if got.Node.Identity.NodeID != "node-high" {
		t.Fatalf("expected node-high (more available memory), got %s", got.Node.Identity.NodeID)
	}
}

func TestSelectNodeDeterministicRegardlessOfInputOrder(t *testing.T) {
	a := nodeWithProfile("node-a", 4, 4<<30, 10, true)
	b := nodeWithProfile("node-b", 4, 8<<30, 10, true)
	c := nodeWithProfile("node-c", 4, 2<<30, 10, true)

	got1, err := selectNode([]*NodeRecord{a, b, c}, domain.ResourceRequirements{})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	got2, err := selectNode([]*NodeRecord{c, a, b}, domain.ResourceRequirements{})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	if got1.Node.Identity.NodeID != got2.Node.Identity.NodeID {
		t.Fatalf("expected the same winner regardless of input order, got %s vs %s", got1.Node.Identity.NodeID, got2.Node.Identity.NodeID)
	}
	if got1.Node.Identity.NodeID != "node-b" {
		t.Fatalf("expected node-b (most available memory), got %s", got1.Node.Identity.NodeID)
	}
}

func TestSelectNodeTieBreaksByNodeID(t *testing.T) {
	z := nodeWithProfile("node-z", 4, 4<<30, 10, true)
	a := nodeWithProfile("node-a", 4, 4<<30, 10, true)

	got, err := selectNode([]*NodeRecord{z, a}, domain.ResourceRequirements{})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	if got.Node.Identity.NodeID != "node-a" {
		t.Fatalf("expected node-a to win the tie (lower NodeID), got %s", got.Node.Identity.NodeID)
	}
}

func TestSelectNodeFiltersOnMinMemoryBytes(t *testing.T) {
	small := nodeWithProfile("node-small", 4, 1<<30, 10, true)
	big := nodeWithProfile("node-big", 4, 8<<30, 10, true)

	got, err := selectNode([]*NodeRecord{small, big}, domain.ResourceRequirements{MinMemoryBytes: 4 << 30})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	if got.Node.Identity.NodeID != "node-big" {
		t.Fatalf("expected only node-big to satisfy MinMemoryBytes, got %s", got.Node.Identity.NodeID)
	}
}

func TestSelectNodeFiltersOnMinCPUCores(t *testing.T) {
	weak := nodeWithProfile("node-weak", 2, 8<<30, 10, true)
	strong := nodeWithProfile("node-strong", 16, 8<<30, 10, true)

	got, err := selectNode([]*NodeRecord{weak, strong}, domain.ResourceRequirements{MinCPUCores: 8})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	if got.Node.Identity.NodeID != "node-strong" {
		t.Fatalf("expected only node-strong to satisfy MinCPUCores, got %s", got.Node.Identity.NodeID)
	}
}

func TestSelectNodeFailsClosedOnMissingDeclaredResource(t *testing.T) {
	noCPUDeclared := &NodeRecord{
		Node:        domain.Node{Identity: domain.Identity{NodeID: "node-nocpu"}},
		Resources:   nil,
		LastMetrics: domain.RuntimeState{MemoryAvailableBytes: 8 << 30, LastHeartbeat: time.Now()},
	}

	_, err := selectNode([]*NodeRecord{noCPUDeclared}, domain.ResourceRequirements{MinCPUCores: 1})
	if !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("expected ErrNoEligibleNode for a node with no declared cpu.cores, got %v", err)
	}
}

func TestSelectNodeFiltersOnMaxCPUPercent(t *testing.T) {
	busy := nodeWithProfile("node-busy", 4, 8<<30, 95, true)
	idle := nodeWithProfile("node-idle", 4, 8<<30, 5, true)

	got, err := selectNode([]*NodeRecord{busy, idle}, domain.ResourceRequirements{MaxCPUPercent: 50})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	if got.Node.Identity.NodeID != "node-idle" {
		t.Fatalf("expected only node-idle to satisfy MaxCPUPercent, got %s", got.Node.Identity.NodeID)
	}
}

// TestSelectNodeTreatsNoHeartbeatAsIneligibleForLiveRequirements proves the
// zero-value-LastMetrics trap: a node that hasn't heartbeated yet must not
// be treated as idle (CPUPercent==0) or as having no memory pressure
// (MemoryAvailableBytes==0 would fail MinMemoryBytes anyway, but for the
// wrong reason — "no data" is not the same as "not enough").
func TestSelectNodeTreatsNoHeartbeatAsIneligibleForLiveRequirements(t *testing.T) {
	neverHeartbeated := nodeWithProfile("node-fresh", 4, 0, 0, false)
	established := nodeWithProfile("node-established", 4, 8<<30, 10, true)

	got, err := selectNode([]*NodeRecord{neverHeartbeated, established}, domain.ResourceRequirements{MaxCPUPercent: 50})
	if err != nil {
		t.Fatalf("selectNode: %v", err)
	}
	if got.Node.Identity.NodeID != "node-established" {
		t.Fatalf("expected the never-heartbeated node excluded despite CPUPercent==0 looking idle, got %s", got.Node.Identity.NodeID)
	}
}

// TestSelectNodeMinCPUCoresIgnoresMissingHeartbeat proves MinCPUCores
// (a static, registration-time requirement) is NOT gated on having a
// heartbeat yet, unlike the live-metric requirements above.
func TestSelectNodeMinCPUCoresIgnoresMissingHeartbeat(t *testing.T) {
	neverHeartbeated := nodeWithProfile("node-fresh", 8, 0, 0, false)

	got, err := selectNode([]*NodeRecord{neverHeartbeated}, domain.ResourceRequirements{MinCPUCores: 4})
	if err != nil {
		t.Fatalf("expected a freshly-registered node to satisfy a static MinCPUCores requirement, got: %v", err)
	}
	if got.Node.Identity.NodeID != "node-fresh" {
		t.Fatalf("expected node-fresh, got %s", got.Node.Identity.NodeID)
	}
}

func TestSelectNodeNoCandidatesReturnsErrNoReadyNode(t *testing.T) {
	_, err := selectNode(nil, domain.ResourceRequirements{})
	if !errors.Is(err, ErrNoReadyNode) {
		t.Fatalf("expected ErrNoReadyNode for an empty candidate list, got %v", err)
	}
}

// TestResolveWorkloadTargetIntegratesWithRegistry confirms the full
// Registry -> resolveWorkloadTarget wiring picks the right node using the
// registry's own mutation methods, not hand-built NodeRecord literals.
func TestResolveWorkloadTargetIntegratesWithRegistry(t *testing.T) {
	r := NewRegistry()
	connA := &fakeConn{tag: "a"}
	connB := &fakeConn{tag: "b"}

	r.Upsert(testNode("node-a"), connA)
	r.SetState("node-a", domain.NodeReady)
	r.UpdateResources("node-a", []domain.Resource{{Kind: domain.ResourceCPUCores, Capacity: 4, Unit: "cores"}}, nil)
	r.RecordHeartbeat("node-a", domain.RuntimeState{MemoryAvailableBytes: 2 << 30, CPUPercent: 10, LastHeartbeat: time.Now()})

	r.Upsert(testNode("node-b"), connB)
	r.SetState("node-b", domain.NodeReady)
	r.UpdateResources("node-b", []domain.Resource{{Kind: domain.ResourceCPUCores, Capacity: 8, Unit: "cores"}}, nil)
	r.RecordHeartbeat("node-b", domain.RuntimeState{MemoryAvailableBytes: 6 << 30, CPUPercent: 10, LastHeartbeat: time.Now()})

	s := &Server{Registry: r}

	rec, id, err := s.resolveWorkloadTarget("", domain.ResourceRequirements{MinMemoryBytes: 4 << 30})
	if err != nil {
		t.Fatalf("resolveWorkloadTarget: %v", err)
	}
	if id != "node-b" {
		t.Fatalf("expected node-b (only one with enough memory), got %s", id)
	}
	if rec.Node.Identity.NodeID != "node-b" {
		t.Fatalf("expected returned record for node-b, got %s", rec.Node.Identity.NodeID)
	}

	if _, _, err := s.resolveWorkloadTarget("", domain.ResourceRequirements{MinMemoryBytes: 100 << 30}); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("expected ErrNoEligibleNode when no node has enough memory, got %v", err)
	}

	if _, _, err := s.resolveWorkloadTarget("node-a", domain.ResourceRequirements{MinMemoryBytes: 4 << 30}); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("expected explicit target node-a to be checked against requirements too, got %v", err)
	}
}
