package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// Revocation is the operator's way to take back an admission. Since
// one-time enrollment made the node's persistent Ed25519 identity its
// reconnect credential (handleRegister), simply forgetting a node is not
// enough — a launcher holding the shared pairing token would re-register
// it within one reconnect backoff. So a revocation is a persisted
// denylist entry for the NodeID, checked on every REGISTER before any
// token is even considered.
//
// What it cannot do, and says so in the API and dashboard rather than
// implying otherwise:
//   - It denies one identity. Anyone holding the shared pairing token can
//     mint a fresh identity and register again; fully cutting off such a
//     device also means rotating -pairing-token (one-time invitations are
//     unaffected — they never embed it).
//   - A relay device credential (an "ha1." alias) still reaches this
//     manager's relay session until the relay's -alias-key is rotated. The
//     manager rejects it at REGISTER, so all it buys is a pool slot.

var (
	// ErrUnknownNode is returned when revoking a NodeID the manager has
	// never admitted (and that isn't already revoked).
	ErrUnknownNode = errors.New("manager: unknown node")
	// ErrNotRevoked is returned when lifting a revocation that doesn't
	// exist.
	ErrNotRevoked = errors.New("manager: node is not revoked")
)

// revokedReason is the REGISTER_REJECT reason a revoked identity receives
// — distinct from "invalid pairing token" so the agent's log says what
// actually happened.
const revokedReason = "node revoked by operator"

// revocationList is the in-memory mirror of the persisted denylist,
// consulted on every REGISTER and every inbound message.
type revocationList struct {
	mu      sync.RWMutex
	entries map[domain.NodeID]domain.RevokedNode
}

func newRevocationList() *revocationList {
	return &revocationList{entries: make(map[domain.NodeID]domain.RevokedNode)}
}

func (l *revocationList) get(id domain.NodeID) (domain.RevokedNode, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.entries[id]
	return r, ok
}

func (l *revocationList) has(id domain.NodeID) bool {
	_, ok := l.get(id)
	return ok
}

func (l *revocationList) put(r domain.RevokedNode) {
	l.mu.Lock()
	l.entries[r.NodeID] = r
	l.mu.Unlock()
}

func (l *revocationList) remove(id domain.NodeID) {
	l.mu.Lock()
	delete(l.entries, id)
	l.mu.Unlock()
}

func (l *revocationList) list() []domain.RevokedNode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]domain.RevokedNode, 0, len(l.entries))
	for _, r := range l.entries {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RevokedAt.After(out[j].RevokedAt) })
	return out
}

// RevokeNode permanently refuses id: it is persisted to the denylist,
// dropped from the registry (so placement can never pick it again), its
// live connection is closed, and its work is wound down — in-flight
// unpinned workloads fail (and may be re-placed elsewhere by their
// restart policy, like any node loss), while workloads pinned to it are
// canceled outright, since their only allowed target is gone for good
// and the reconciler would otherwise defer them forever. Revoking an
// already-revoked node returns the existing entry.
func (s *Server) RevokeNode(ctx context.Context, id domain.NodeID) (domain.RevokedNode, error) {
	s.admitMu.Lock()
	if existing, ok := s.revocations.get(id); ok {
		s.admitMu.Unlock()
		return existing, nil
	}
	rec, ok := s.Registry.Get(id)
	if !ok {
		s.admitMu.Unlock()
		return domain.RevokedNode{}, ErrUnknownNode
	}
	revoked := domain.RevokedNode{
		NodeID:    id,
		Name:      rec.Node.Name,
		Hostname:  rec.Node.Hostname,
		Platform:  rec.Node.Platform,
		RevokedAt: time.Now().UTC(),
	}
	// Persist before acting: a revocation the operator was told succeeded
	// must still hold after a manager restart, so a failed write aborts
	// with nothing changed rather than revoking only until the next boot.
	if s.store != nil {
		if err := s.store.RevokeNode(revoked); err != nil {
			s.admitMu.Unlock()
			return domain.RevokedNode{}, fmt.Errorf("manager: persist revocation of %s: %w", id, err)
		}
	}
	s.revocations.put(revoked)
	removed, _ := s.Registry.Remove(id)
	s.admitMu.Unlock()

	// Closing the connection makes the agent cancel whatever it is running
	// (Agent.Run calls executor.CancelCurrent on every disconnect) and
	// enter its reconnect loop, where REGISTER now fails with
	// revokedReason. Close runs in the background: a WebSocket close can
	// wait seconds for a handshake ack the peer may never send.
	if removed != nil && removed.Conn != nil {
		go removed.Conn.Close()
	}
	s.failPendingCommandsFor(id, revokedReason)
	for _, wrec := range s.Workloads.CancelPinnedTo(id, revokedReason) {
		s.persistWorkloadRecord(wrec)
		log.Printf("workload.canceled: %s (%s)", wrec.Workload.ID, revokedReason)
		s.publish(domain.EventWorkloadCanceled, id, map[string]any{"workloadId": string(wrec.Workload.ID), "reason": revokedReason})
	}
	s.failWorkloadsFor(ctx, id, revokedReason)

	log.Printf("node.revoked: %s (%s)", id, revoked.Name)
	s.publish(domain.EventNodeRevoked, id, map[string]any{"name": revoked.Name})
	return revoked, nil
}

// UnrevokeNode lifts a revocation. It does not re-admit the node: the
// identity is unknown again, so its next REGISTER needs a valid admission
// credential — the shared pairing token (which a `harnessctl join`
// launcher still holds, so that node returns on its own) or a fresh
// one-time invitation (whose consumed token in the old launcher no longer
// works, so re-run the new invitation's setup on that device).
func (s *Server) UnrevokeNode(id domain.NodeID) error {
	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	if !s.revocations.has(id) {
		return ErrNotRevoked
	}
	if s.store != nil {
		if err := s.store.UnrevokeNode(id); err != nil {
			return fmt.Errorf("manager: persist unrevocation of %s: %w", id, err)
		}
	}
	s.revocations.remove(id)
	log.Printf("node.unrevoked: %s", id)
	s.publish(domain.EventNodeUnrevoked, id, nil)
	return nil
}

// RevokedNodes lists current revocations, most recent first.
func (s *Server) RevokedNodes() []domain.RevokedNode { return s.revocations.list() }

func (s *Server) apiRevokeNode(w http.ResponseWriter, r *http.Request) {
	revoked, err := s.RevokeNode(r.Context(), domain.NodeID(r.PathValue("id")))
	switch {
	case errors.Is(err, ErrUnknownNode):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, revoked)
	}
}

func (s *Server) apiListRevocations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.RevokedNodes())
}

func (s *Server) apiUnrevokeNode(w http.ResponseWriter, r *http.Request) {
	err := s.UnrevokeNode(domain.NodeID(r.PathValue("id")))
	switch {
	case errors.Is(err, ErrNotRevoked):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
