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

// NodeRecord is the registry's in-memory view of one node: its manifest-
// derived identity/metadata plus current runtime state. Persistent vs
// runtime storage is layered in a later milestone (v1.md §13); for the
// walking skeleton both live here, in memory only.
type NodeRecord struct {
	Node     domain.Node
	State    domain.NodeState
	LastSeen time.Time
	Conn     domain.Conn
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

// Upsert inserts or replaces the record for a node's identity, returning
// whether this is a previously unknown node.
func (r *Registry) Upsert(node domain.Node, conn domain.Conn) (rec *NodeRecord, isNew bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := node.Identity.NodeID
	existing, ok := r.nodes[id]
	if !ok {
		rec := &NodeRecord{Node: node, State: domain.NodeConnected, LastSeen: time.Now(), Conn: conn}
		r.nodes[id] = rec
		return rec, true
	}
	existing.Node = node
	existing.Conn = conn
	existing.LastSeen = time.Now()
	return existing, false
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

// Touch records a heartbeat/activity timestamp for a node.
func (r *Registry) Touch(id domain.NodeID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok {
		rec.LastSeen = time.Now()
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
