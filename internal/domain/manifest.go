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
}
