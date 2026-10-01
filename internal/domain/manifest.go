package domain

// ManifestSchemaVersion is versioned independently of the protocol envelope
// version, since the manifest shape can evolve (new resource/capability
// kinds) without changing how messages are framed.
const ManifestSchemaVersion = "v0.1"

// Manifest is what a node publishes about itself: identity, metadata,
// resources, and capabilities. The schema must stay extensible — see
// Home_Compute_Harness_Baseline_Architecture.md §7 and v1.md §5.
type Manifest struct {
	SchemaVersion string       `json:"schemaVersion"`
	Node          Node         `json:"node"`
	Resources     []Resource   `json:"resources"`
	Capabilities  []Capability `json:"capabilities"`
	// AgentFeatures lists agent-protocol behaviors this agent build
	// supports, so the manager can treat a mixed-version fleet correctly
	// instead of assuming every agent is current. Absent (an older agent)
	// means none: the manager must fall back to legacy behavior. Distinct
	// from Capabilities, which are workload types a node can execute.
	AgentFeatures []string `json:"agentFeatures,omitempty"`
	// WorkloadSlots is how many workloads this agent runs at once. Absent
	// (an older agent) means 1, which is exactly how those agents behave.
	WorkloadSlots int `json:"workloadSlots,omitempty"`
}

// Slots is the manifest's workload slot count, treating absent as 1.
func (m Manifest) Slots() int {
	if m.WorkloadSlots < 1 {
		return 1
	}
	return m.WorkloadSlots
}

// FeatureSelfUpdatePath means the agent downloads a self-update from the
// path the SELF_UPDATE command names (a per-platform catalog entry),
// rather than always from the legacy single /agent-binary route.
const FeatureSelfUpdatePath = "self-update.path"

// FeatureTimeout means the agent enforces Workload.TimeoutSeconds.
// Placement only sends a workload with a timeout to agents advertising
// it: an older agent would silently run it without one.
const FeatureTimeout = "timeout.v1"

// HasAgentFeature reports whether m advertises feature.
func (m Manifest) HasAgentFeature(feature string) bool {
	for _, f := range m.AgentFeatures {
		if f == feature {
			return true
		}
	}
	return false
}
