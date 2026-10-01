package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"home-harness/internal/domain"
)

// --- Operator metadata: aliases and labels ---------------------------

// nodeMetaStore is the in-memory mirror of persisted operator metadata,
// keyed by NodeID and independent of the registry: it outlives a node
// going offline and is cleared only by revocation.
type nodeMetaStore struct {
	mu      sync.RWMutex
	entries map[domain.NodeID]domain.NodeMeta
}

func newNodeMetaStore() *nodeMetaStore {
	return &nodeMetaStore{entries: make(map[domain.NodeID]domain.NodeMeta)}
}

func (m *nodeMetaStore) get(id domain.NodeID) domain.NodeMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entries[id]
}

func (m *nodeMetaStore) set(id domain.NodeID, meta domain.NodeMeta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if meta.Empty() {
		delete(m.entries, id)
		return
	}
	m.entries[id] = meta
}

var labelKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

const (
	maxAliasLen      = 64
	maxLabelValueLen = 128
	maxLabels        = 32
)

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// validateNodeMeta keeps aliases and labels short, printable, and (for
// keys) selector-safe, so labels can become placement selectors later.
// The dashboard still escapes them regardless.
func validateNodeMeta(meta domain.NodeMeta) error {
	if utf8.RuneCountInString(meta.Alias) > maxAliasLen || !printable(meta.Alias) {
		return fmt.Errorf("alias must be at most %d printable characters", maxAliasLen)
	}
	if len(meta.Labels) > maxLabels {
		return fmt.Errorf("at most %d labels", maxLabels)
	}
	for k, v := range meta.Labels {
		if !labelKeyPattern.MatchString(k) {
			return fmt.Errorf("label key %q must match %s", k, labelKeyPattern)
		}
		if v == "" || utf8.RuneCountInString(v) > maxLabelValueLen || !printable(v) {
			return fmt.Errorf("label %q value must be 1-%d printable characters", k, maxLabelValueLen)
		}
	}
	return nil
}

// SetNodeMeta replaces the operator metadata for a known node.
func (s *Server) SetNodeMeta(id domain.NodeID, meta domain.NodeMeta) (old domain.NodeMeta, err error) {
	meta.Alias = strings.TrimSpace(meta.Alias)
	if err := validateNodeMeta(meta); err != nil {
		return domain.NodeMeta{}, err
	}
	if _, ok := s.Registry.Get(id); !ok {
		return domain.NodeMeta{}, ErrUnknownNode
	}
	if s.store != nil {
		if err := s.store.PutNodeMeta(id, meta); err != nil {
			return domain.NodeMeta{}, fmt.Errorf("manager: persist node meta: %w", err)
		}
	}
	old = s.meta.get(id)
	s.meta.set(id, meta)
	return old, nil
}

func (s *Server) apiPutNodeMeta(w http.ResponseWriter, r *http.Request) {
	id := domain.NodeID(r.PathValue("id"))
	var meta domain.NodeMeta
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&meta); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	old, err := s.SetNodeMeta(id, meta)
	switch {
	case errors.Is(err, ErrUnknownNode):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(domain.AuditSecurity, "node.meta-changed", id, actorFrom(r.Context()), map[string]any{"from": old, "to": s.meta.get(id)})
	s.publish(domain.EventNodeUpdated, id, map[string]any{"reason": "operator metadata changed"})
	writeJSON(w, http.StatusOK, s.meta.get(id))
}

// --- Same-host detection --------------------------------------------

// hostRelation is how one node relates to others reporting its machine.
type hostRelation struct {
	sameHostAs []domain.NodeID
	// conflict: the fingerprints match but the declared hardware doesn't,
	// so this is not corroborated as one machine (a cloned VM image, a
	// copied fingerprint) and capacity is counted per identity.
	conflict bool
}

// resourceSignature is the declared hardware that must also match before
// identities sharing a fingerprint are treated as one machine.
func resourceSignature(rec *NodeRecord) string {
	var cores, memory float64
	for _, r := range rec.Resources {
		switch r.Kind {
		case domain.ResourceCPUCores:
			cores = r.Capacity
		case domain.ResourceMemoryBytes:
			memory = r.Capacity
		}
	}
	return fmt.Sprintf("%g/%g", cores, memory)
}

// hostRelations groups records by host fingerprint. The fingerprint is an
// untrusted, agent-asserted hint, so groups are only *corroborated* (and
// later collapsed in totals) when every member also declares identical
// cores and memory; otherwise the group is flagged as a conflict. Nothing
// automatic acts on either — they're for the operator to look at.
func hostRelations(records []*NodeRecord) map[domain.NodeID]hostRelation {
	groups := make(map[string][]*NodeRecord)
	for _, rec := range records {
		if fp := rec.Node.HostFingerprint; fp != "" {
			groups[fp] = append(groups[fp], rec)
		}
	}
	out := make(map[domain.NodeID]hostRelation)
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		conflict := false
		for _, m := range members[1:] {
			if resourceSignature(m) != resourceSignature(members[0]) {
				conflict = true
			}
		}
		for _, m := range members {
			var others []domain.NodeID
			for _, o := range members {
				if o != m {
					others = append(others, o.Node.Identity.NodeID)
				}
			}
			sort.Slice(others, func(i, j int) bool { return others[i] < others[j] })
			out[m.Node.Identity.NodeID] = hostRelation{sameHostAs: others, conflict: conflict}
		}
	}
	return out
}

// dedupedForTotals returns the records whose resources count toward
// fleet totals: one per corroborated same-host group (the lowest NodeID,
// for determinism), every member of a conflicting group, and every
// unfingerprinted node.
func dedupedForTotals(records []*NodeRecord) []*NodeRecord {
	relations := hostRelations(records)
	var out []*NodeRecord
	for _, rec := range records {
		rel, grouped := relations[rec.Node.Identity.NodeID]
		if grouped && !rel.conflict && rel.sameHostAs[0] < rec.Node.Identity.NodeID {
			continue // a lower-ID identity of the same machine already counts
		}
		out = append(out, rec)
	}
	return out
}
