// Package manager implements the control-plane side of the harness:
// registration, the node registry, heartbeat monitoring, and (in later
// milestones) resource/capability registries, command dispatch, and the
// HTTP API. It depends on domain and protocol, never on a concrete
// transport package.
package manager

import (
	"sync"
	"time"

	"home-harness/internal/domain"
)

// NodeRecord is the registry's in-memory view of one node: its last-known
// manifest (identity/metadata plus declared resources/capabilities) plus
// current runtime state.
type NodeRecord struct {
	Node         domain.Node
	Resources    []domain.Resource
	Capabilities []domain.Capability
	State        domain.NodeState
	LastSeen     time.Time
	Conn         domain.Conn
	// LastMetrics is the most recently reported live CPU/memory figures
	// from a HEARTBEAT, distinct from the static Resources declared at
	// registration (v1.md §13's runtime vs persistent state split).
	LastMetrics domain.RuntimeState
}

// Registry tracks all nodes the manager currently knows about.
type Registry struct {
	mu    sync.RWMutex
	nodes map[domain.NodeID]*NodeRecord
}

// NewRegistry returns an empty node registry.
func NewRegistry() *Registry {
	return &Registry{nodes: make(map[domain.NodeID]*NodeRecord)}
}

// Upsert inserts or replaces the record for a node's identity from its
// manifest, returning whether this is a previously unknown node.
func (r *Registry) Upsert(manifest domain.Manifest, conn domain.Conn) (rec *NodeRecord, isNew bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := manifest.Node.Identity.NodeID
	existing, ok := r.nodes[id]
	if !ok {
		rec := &NodeRecord{
			Node:         manifest.Node,
			Resources:    manifest.Resources,
			Capabilities: manifest.Capabilities,
			State:        domain.NodeConnected,
			LastSeen:     time.Now(),
			Conn:         conn,
		}
		r.nodes[id] = rec
		return rec, true
	}
	existing.Node = manifest.Node
	existing.Resources = manifest.Resources
	existing.Capabilities = manifest.Capabilities
	existing.Conn = conn
	existing.LastSeen = time.Now()
	// A reconnect (new process, new connection) invalidates any previous
	// live metrics — LastSeen alone doesn't catch this, since it's
	// refreshed by this same call. Without clearing LastMetrics here, a
	// resource-aware placement decision (nodeFits) could trust a reading
	// from a since-restarted process as if it were current, until the
	// first fresh heartbeat overwrites it.
	existing.LastMetrics = domain.RuntimeState{}
	return existing, false
}

// UpdateResources replaces a known node's declared resources/capabilities,
// e.g. after a CAPABILITY_UPDATE following REQUEST_RESOURCE_REFRESH.
func (r *Registry) UpdateResources(id domain.NodeID, resources []domain.Resource, capabilities []domain.Capability) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok {
		rec.Resources = resources
		rec.Capabilities = capabilities
	}
}

// TotalResources sums each resource kind across every known node, e.g. for
// the harness-wide "Total visible resources" view (v1.md §11). The number
// is informational only in v0 — nothing pools or schedules against it.
func (r *Registry) TotalResources() map[domain.ResourceKind]float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	totals := make(map[domain.ResourceKind]float64)
	for _, rec := range r.nodes {
		for _, res := range rec.Resources {
			totals[res.Kind] += res.Capacity
		}
	}
	return totals
}

// Get returns the record for a node ID, if known.
func (r *Registry) Get(id domain.NodeID) (*NodeRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.nodes[id]
	return rec, ok
}

// SetState updates a known node's lifecycle state.
func (r *Registry) SetState(id domain.NodeID, state domain.NodeState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok {
		rec.State = state
	}
}

// SetOfflineIfCurrent transitions a node to OFFLINE only if conn is still
// the record's active connection. This guards against a stale connection's
// own cleanup (its Receive loop finally erroring out after the node has
// already reconnected on a new connection) clobbering a live reconnect —
// churn is normal (baseline §9 rule 4), and a slow-to-notice dead
// connection must not undo a newer, already-registered one. It reports
// whether it actually transitioned the node.
func (r *Registry) SetOfflineIfCurrent(id domain.NodeID, conn domain.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[id]
	if !ok || rec.Conn != conn {
		return false
	}
	rec.State = domain.NodeOffline
	return true
}

// Touch records a heartbeat/activity timestamp for a node.
func (r *Registry) Touch(id domain.NodeID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok {
		rec.LastSeen = time.Now()
	}
}

// RecordHeartbeat updates a node's last-seen timestamp and its most
// recently reported live metrics in one step.
func (r *Registry) RecordHeartbeat(id domain.NodeID, metrics domain.RuntimeState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok {
		rec.LastSeen = time.Now()
		rec.LastMetrics = metrics
	}
}

// List returns a snapshot of all known nodes.
func (r *Registry) List() []*NodeRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*NodeRecord, 0, len(r.nodes))
	for _, rec := range r.nodes {
		out = append(out, rec)
	}
	return out
}

// Seed inserts a node as OFFLINE with no live connection, without
// overwriting an existing (possibly already-live) record. It is how the
// manager restores previously-registered nodes into the runtime registry
// after a restart, before any of them have reconnected (v1.md §13/§21:
// state survives manager restart where appropriate).
func (r *Registry) Seed(manifest domain.Manifest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := manifest.Node.Identity.NodeID
	if _, exists := r.nodes[id]; exists {
		return
	}
	r.nodes[id] = &NodeRecord{
		Node:         manifest.Node,
		Resources:    manifest.Resources,
		Capabilities: manifest.Capabilities,
		State:        domain.NodeOffline,
	}
}

// ExpireStale transitions any node whose LastSeen exceeds timeout into
// OFFLINE, returning the IDs that were just transitioned so callers can
// emit node.offline events without holding the registry lock.
func (r *Registry) ExpireStale(timeout time.Duration) []domain.NodeID {
	r.mu.Lock()
	defer r.mu.Unlock()

	var expired []domain.NodeID
	now := time.Now()
	for id, rec := range r.nodes {
		if rec.State == domain.NodeOffline {
			continue
		}
		if now.Sub(rec.LastSeen) > timeout {
			rec.State = domain.NodeOffline
			expired = append(expired, id)
		}
	}
	return expired
}
