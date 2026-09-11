package domain

// ResourceKind is an open string type (not a closed enum) so new resource
// kinds — GPU, NPU, battery, bandwidth — can be introduced by extension
// packages without changing this type.
type ResourceKind string

const (
	ResourceCPUCores     ResourceKind = "cpu.cores"
	ResourceMemoryBytes  ResourceKind = "memory.bytes"
	ResourceStorageBytes ResourceKind = "storage.bytes"
)

// Resource is a single quantified capacity a node exposes. Capacity is a
// plain numeric magnitude in Unit, so registries can sum resources of the
// same Kind across nodes without knowing what the kind means.
type Resource struct {
	Kind     ResourceKind `json:"kind"`
	Capacity float64      `json:"capacity"`
	Unit     string       `json:"unit"`
}
