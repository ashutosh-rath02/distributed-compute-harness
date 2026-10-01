package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Policy (roadmap item 7): what the fleet may be asked to run. Every
// submission path goes through Submit, which checks it — the API, job
// attempts, and later the AI planner — and restarts re-check it, so
// disabling a type also stops its services from coming back.

// ErrPolicy is a submission the operator's policy forbids.
var ErrPolicy = errors.New("manager: blocked by policy")

const maxPolicyRuntime = 7 * 24 * 3600

type policyStore struct {
	mu sync.RWMutex
	p  domain.Policy
}

func (ps *policyStore) get() domain.Policy {
	if ps == nil { // a bare Server (unit tests): the library default
		return domain.PermissivePolicy()
	}
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return clonePolicy(ps.p)
}

func (ps *policyStore) set(p domain.Policy) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.p = clonePolicy(p)
}

func clonePolicy(p domain.Policy) domain.Policy {
	out := domain.Policy{AllowUnlisted: p.AllowUnlisted, Types: make(map[domain.CapabilityName]domain.TypePolicy, len(p.Types))}
	for k, v := range p.Types {
		labels := make(map[string]string, len(v.NodeLabels))
		for lk, lv := range v.NodeLabels {
			labels[lk] = lv
		}
		v.NodeLabels = labels
		out.Types[k] = v
	}
	return out
}

// effectivePolicy is the rule that applies to capability.
func effectivePolicy(p domain.Policy, capability domain.CapabilityName) domain.TypePolicy {
	if tp, ok := p.Types[capability]; ok {
		return tp
	}
	if _, ok := catalog.Lookup(capability); ok {
		return domain.TypePolicy{Enabled: true}
	}
	return domain.TypePolicy{Enabled: p.AllowUnlisted}
}

// policyFor is the rule for capability under the current policy.
func (s *Server) policyFor(capability domain.CapabilityName) domain.TypePolicy {
	return effectivePolicy(s.policy.get(), capability)
}

// checkPolicy refuses a capability the policy disables, saying how to
// turn it on.
func (s *Server) checkPolicy(capability domain.CapabilityName) error {
	if s.policyFor(capability).Enabled {
		return nil
	}
	if catalog.IsRaw(capability) {
		return fmt.Errorf("%w: raw %s is off; it is an advanced opt-in — turn it on with \"harnessctl policy type %s on\" (optionally only for labelled devices: \"harnessctl policy type %s labels raw=ok\"), or in the dashboard's Policy section", ErrPolicy, capability, capability, capability)
	}
	return fmt.Errorf("%w: %s is disabled — \"harnessctl policy type %s on\" enables it", ErrPolicy, capability, capability)
}

func validatePolicy(p domain.Policy) error {
	for name, tp := range p.Types {
		if _, ok := catalog.Lookup(name); !ok && !catalog.IsRaw(name) {
			return fmt.Errorf("unknown capability %q", name)
		}
		if tp.MaxRuntimeSeconds < 0 || tp.MaxRuntimeSeconds > maxPolicyRuntime {
			return fmt.Errorf("%s: maxRuntimeSeconds must be 0-%d", name, maxPolicyRuntime)
		}
		if len(tp.NodeLabels) > maxLabels {
			return fmt.Errorf("%s: at most %d node labels", name, maxLabels)
		}
		for k, v := range tp.NodeLabels {
			if !labelKeyPattern.MatchString(k) || v == "" || len(v) > maxLabelValueLen || !printable(v) {
				return fmt.Errorf("%s: invalid node label %q=%q", name, k, v)
			}
		}
	}
	return nil
}

// SetPolicy replaces the policy (validated, persisted, audited by the
// caller). It applies to new submissions and restarts; already-queued
// work was accepted under the old one and runs.
func (s *Server) SetPolicy(p domain.Policy) (old domain.Policy, err error) {
	if err := validatePolicy(p); err != nil {
		return domain.Policy{}, err
	}
	if s.store != nil {
		if err := s.store.PutPolicy(p); err != nil {
			return domain.Policy{}, fmt.Errorf("manager: persist policy: %w", err)
		}
	}
	old = s.policy.get()
	s.policy.set(p)
	return old, nil
}

// matchesLabels reports whether node carries every selector label.
func (s *Server) matchesLabels(node domain.NodeID, selector map[string]string) bool {
	if len(selector) == 0 {
		return true
	}
	have := s.meta.get(node).Labels
	for k, v := range selector {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ---- API

func (s *Server) apiGetPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.policy.get())
}

func (s *Server) apiPutPolicy(w http.ResponseWriter, r *http.Request) {
	var p domain.Policy
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	old, err := s.SetPolicy(p)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(domain.AuditSecurity, "policy.changed", "", actorFrom(r.Context()), map[string]any{"old": old, "new": p})
	log.Printf("policy.changed by %s", actorFrom(r.Context()))
	writeJSON(w, http.StatusOK, s.policy.get())
}

// catalogEntry is one catalog type as the API shows it.
type catalogEntry struct {
	catalog.Type
	Policy domain.TypePolicy `json:"policy"`
	// Nodes is how many READY nodes offer this type (at this version)
	// and pass its policy's label selector.
	Nodes int `json:"nodes"`
}

func (s *Server) apiGetCatalog(w http.ResponseWriter, r *http.Request) {
	p := s.policy.get()
	var out []catalogEntry
	for _, t := range catalog.Types() {
		e := catalogEntry{Type: t, Policy: effectivePolicy(p, t.Name)}
		for _, rec := range s.Registry.List() {
			if rec.State == domain.NodeReady && offersVersion(rec, t.Name) && s.matchesLabels(rec.Node.Identity.NodeID, e.Policy.NodeLabels) {
				e.Nodes++
			}
		}
		out = append(out, e)
	}
	raw := map[domain.CapabilityName]domain.TypePolicy{}
	for _, name := range []domain.CapabilityName{domain.CapabilitySystemExecute, domain.CapabilityFilesystemRead} {
		raw[name] = effectivePolicy(p, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": out, "raw": raw})
}

// offersVersion reports whether rec advertises capability at the
// catalog's version (any version for a capability outside the catalog).
func offersVersion(rec *NodeRecord, capability domain.CapabilityName) bool {
	t, inCatalog := catalog.Lookup(capability)
	for _, c := range rec.Capabilities {
		if c.Name == capability {
			return !inCatalog || c.Version == t.Version
		}
	}
	return false
}
