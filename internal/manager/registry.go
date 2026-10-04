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
	// AgentFeatures is the manifest's agent-protocol feature list (see
	// domain.Manifest.AgentFeatures); empty for an agent predating it.
	AgentFeatures []string
	// GPUs the device reported (domain.Manifest.GPUs).
	GPUs     []domain.GPU
	State    domain.NodeState
	LastSeen time.Time
	Conn     domain.Conn
	// slotsN is the node's advertised workload slot count (Manifest.Slots:
	// absent = 1). busyUntil holds placement off a node that just refused
	// work for lack of a free slot: until then, or until it next finishes
	// a workload, whichever is first (queue.go).
	slotsN    int
	busyUntil time.Time
	// cached is which input files the node says it holds (locality.go):
	// SHA-256 prefixes, replaced whole by each report, never mutated.
	cached map[string]struct{}
	// LastMetrics is the most recently reported live CPU/memory figures
	// from a HEARTBEAT, distinct from the static Resources declared at
	// registration (v1.md §13's runtime vs persistent state split).
	LastMetrics domain.RuntimeState
}

// slots is how many workloads the node runs at once.
func (rec *NodeRecord) slots() int {
	if rec.slotsN < 1 {
		return 1
	}
	return rec.slotsN
}

// holdOffBusy keeps placement off a node that refused work for lack of
// slots until until; the zero time clears it (it finished something).
func (r *Registry) holdOffBusy(id domain.NodeID, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok {
		rec.busyUntil = until
	}
}

// hasAgentFeature reports whether rec's last manifest advertised feature.
func (rec *NodeRecord) hasAgentFeature(feature string) bool {
	for _, f := range rec.AgentFeatures {
		if f == feature {
			return true
		}
	}
	return false
}

// HasCapability reports whether rec's last-known manifest declares name.
func (rec *NodeRecord) HasCapability(name domain.CapabilityName) bool {
	for _, c := range rec.Capabilities {
		if c.Name == name {
			return true
		}
	}
	return false
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
			Node:          manifest.Node,
			Resources:     manifest.Resources,
			Capabilities:  manifest.Capabilities,
			AgentFeatures: manifest.AgentFeatures,
			GPUs:          manifest.GPUs,
			slotsN:        manifest.Slots(),
			State:         domain.NodeConnected,
			LastSeen:      time.Now(),
			Conn:          conn,
		}
		r.nodes[id] = rec
		return rec, true
	}
	existing.Node = manifest.Node
	existing.Resources = manifest.Resources
	existing.Capabilities = manifest.Capabilities
	existing.AgentFeatures = manifest.AgentFeatures
	existing.GPUs = manifest.GPUs
	existing.slotsN = manifest.Slots()
	existing.busyUntil = time.Time{} // a fresh process starts with free slots
	existing.cached = nil            // until this connection reports it
	existing.Conn = conn
	// Not READY until the manager has acknowledged the registration on
	// this connection (handleRegister), even if the old one still was.
	existing.State = domain.NodeConnected
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
	records := make([]*NodeRecord, 0, len(r.nodes))
	for _, rec := range r.nodes {
		records = append(records, rec)
	}
	// Identities corroborated as one machine (fleet.go) count once.
	totals := make(map[domain.ResourceKind]float64)
	for _, rec := range dedupedForTotals(records) {
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

// Remove forgets a node entirely, returning its last record (with any live
// Conn, so the caller can close it). Used by revocation, where the node
// must vanish from listings and placement rather than linger as OFFLINE.
func (r *Registry) Remove(id domain.NodeID) (*NodeRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.nodes[id]
	if ok {
		delete(r.nodes, id)
	}
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
	// Already OFFLINE (a heartbeat timeout got there first, then closed
	// this connection): its cleanup already ran.
	if !ok || rec.Conn != conn || rec.State == domain.NodeOffline {
		return false
	}
	rec.State = domain.NodeOffline
	return true
}

// ConnOf returns a node's current connection (nil if none).
func (r *Registry) ConnOf(id domain.NodeID) domain.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if rec, ok := r.nodes[id]; ok {
		return rec.Conn
	}
	return nil
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

// SetUse records a node's use as it reported it on connecting (its
// heartbeats replace it), so its availability is known before the first
// heartbeat.
func (r *Registry) SetUse(id domain.NodeID, use *domain.DeviceUse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok {
		rec.LastMetrics.Use = use
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
		Node:          manifest.Node,
		Resources:     manifest.Resources,
		Capabilities:  manifest.Capabilities,
		AgentFeatures: manifest.AgentFeatures,
		GPUs:          manifest.GPUs,
		slotsN:        manifest.Slots(),
		State:         domain.NodeOffline,
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
