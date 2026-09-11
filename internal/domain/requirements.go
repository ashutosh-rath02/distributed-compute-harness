package domain

// ResourceRequirements is a workload's optional placement constraints.
// Every field's zero value means "no constraint" — a workload with an
// empty ResourceRequirements places exactly as v1 did (any READY,
// connected node is eligible). This is v2's entire "resource-aware
// placement": a simple filter plus a single-signal preference, not a
// weighted or policy-driven scheduler (v1.md §19 excludes an "AI
// scheduler" from scope).
type ResourceRequirements struct {
	// MinCPUCores is checked against a node's static declared cpu.cores
	// capacity — there is no live "available cores" metric to check
	// against instead.
	MinCPUCores float64 `json:"minCpuCores,omitempty"`
	// MinMemoryBytes is checked against the node's live
	// LastMetrics.MemoryAvailableBytes from its most recent heartbeat.
	MinMemoryBytes uint64 `json:"minMemoryBytes,omitempty"`
	// MaxCPUPercent excludes nodes whose live LastMetrics.CPUPercent
	// currently exceeds this value. Zero means unset, not "must be
	// perfectly idle" — requiring exactly 0.0% CPU would be unsatisfiable
	// on real hardware, so the collision with a genuine 0 constraint is
	// harmless in practice.
	MaxCPUPercent float64 `json:"maxCpuPercent,omitempty"`
}

// IsEmpty reports whether r imposes no constraint at all.
func (r ResourceRequirements) IsEmpty() bool {
	return r.MinCPUCores == 0 && r.MinMemoryBytes == 0 && r.MaxCPUPercent == 0
}

// StaticCapacity returns the declared capacity for kind among res, and
// whether the node declared that kind at all.
func StaticCapacity(res []Resource, kind ResourceKind) (float64, bool) {
	for _, r := range res {
		if r.Kind == kind {
			return r.Capacity, true
		}
	}
	return 0, false
}
