package domain

// CapabilityName is an open string type, extensible by adapters (camera,
// Bluetooth, AI inference, ...) without changing the core domain.
type CapabilityName string

// CapabilitySystemExecute and CapabilityFilesystemRead are declared for
// real by every agent's manifest (internal/sysinfo.Manifest) and enforced
// by the manager at placement time (internal/manager/placement.go) —
// invoking them dispatches to internal/agent/executor.go's
// startExecute/startFilesystemRead respectively. CapabilityFilesystemWrite
// remains reserved: a name that exists for future use, but that no
// manifest declares and no runtime implements yet — capability-first
// modeling (baseline §8 rule 2) means a node is described by what it
// actually exposes, and advertising an unenforceable capability would
// violate deterministic enforcement (baseline §9 rule 5).
const (
	CapabilitySystemExecute   CapabilityName = "system.execute"
	CapabilityFilesystemRead  CapabilityName = "filesystem.read"
	CapabilityFilesystemWrite CapabilityName = "filesystem.write"
)

// Capability is an action or function a node claims to support. Only
// entries a node's manifest actually declares are ever true — see
// CapabilityFilesystemWrite above for a name that exists but nothing yet
// declares.
type Capability struct {
	Name    CapabilityName `json:"name"`
	Version string         `json:"version"`
	// Attributes carry per-node detail placement can match against, e.g.
	// "models" for the local models an llm.generate node has.
	Attributes map[string]string `json:"attributes,omitempty"`
}
