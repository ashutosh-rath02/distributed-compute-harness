package manager

import "home-harness/internal/domain"

// Data locality (roadmap-after-9 item 16). Agents with
// domain.FeatureInputCache keep the input files they downloaded and say
// which (a bounded list of SHA-256 prefixes, on registering and in a
// heartbeat whenever it changed). Placement then prefers, among devices
// equally busy, the one already holding the most bytes of a workload's
// inputs (selectNodeLocal): a second job on the same big file goes where
// the file already is, instead of downloading it to another device.
//
// What an agent says it holds is an untrusted hint. It is only ever a
// tie-breaker within one load level, counted in the sizes the manager
// itself recorded for the inputs (never the agent's), and has no bearing
// on what runs or with which bytes: the agent checks every input's size
// and SHA-256 whether it came from its cache or the network.

// setCached records which input files node says it holds. Reports from a
// node that doesn't advertise FeatureInputCache are ignored (an older
// agent makes none; anything else is not to be trusted with a preference),
// as are malformed entries; at most MaxInputCacheReport entries are read.
func (r *Registry) setCached(id domain.NodeID, report *domain.InputCacheReport) {
	if report == nil {
		return
	}
	prefixes := report.Prefixes
	if len(prefixes) > domain.MaxInputCacheReport {
		prefixes = prefixes[:domain.MaxInputCacheReport]
	}
	set := make(map[string]struct{}, len(prefixes))
	for _, p := range prefixes {
		if validCachePrefix(p) {
			set[p] = struct{}{}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok && rec.hasAgentFeature(domain.FeatureInputCache) {
		rec.cached = set
	}
}

func validCachePrefix(p string) bool {
	if len(p) != domain.InputCachePrefixLen {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// localBytes is, for each of nodes holding any, how many bytes of inputs
// it says it already has: each distinct file once, at the size the
// manager recorded for it. Nil for a workload without files.
func (r *Registry) localBytes(nodes []*NodeRecord, inputs []domain.ArtifactRef) map[domain.NodeID]int64 {
	if len(inputs) == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out map[domain.NodeID]int64
	for _, rec := range nodes {
		if len(rec.cached) == 0 {
			continue
		}
		var held int64
		seen := make(map[string]bool, len(inputs))
		for _, in := range inputs {
			if seen[in.SHA256] || len(in.SHA256) < domain.InputCachePrefixLen {
				continue
			}
			seen[in.SHA256] = true
			if _, ok := rec.cached[in.SHA256[:domain.InputCachePrefixLen]]; ok {
				held += in.Size
			}
		}
		if held > 0 {
			if out == nil {
				out = map[domain.NodeID]int64{}
			}
			out[rec.Node.Identity.NodeID] = held
		}
	}
	return out
}

// HoldsInput reports whether node has said it holds the file with sha256
// (and is trusted to say so): what placement's locality preference reads.
func (r *Registry) HoldsInput(node domain.NodeID, sha256 string) bool {
	if len(sha256) < domain.InputCachePrefixLen {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.nodes[node]
	if !ok {
		return false
	}
	_, held := rec.cached[sha256[:domain.InputCachePrefixLen]]
	return held
}
