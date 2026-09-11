package domain

// CapabilityName is an open string type, extensible by adapters (camera,
// Bluetooth, AI inference, ...) without changing the core domain.
type CapabilityName string

// Reserved for a future runtime that can actually enforce them. No v0
// code declares these in a live manifest — capability-first modeling
// (baseline §8 rule 2) means a node is described by what it actually
// exposes, and advertising an unenforceable capability would violate
// deterministic enforcement (baseline §9 rule 5). See internal/sysinfo.
const (
	CapabilitySystemExecute   CapabilityName = "system.execute"
	CapabilityFilesystemRead  CapabilityName = "filesystem.read"
	CapabilityFilesystemWrite CapabilityName = "filesystem.write"
)

// Capability is an action or function a node claims to support. Only
// entries a node's manifest actually declares are ever true — see the
// reserved constants above for names that exist but nothing yet declares.
type Capability struct {
	Name    CapabilityName `json:"name"`
	Version string         `json:"version"`
}
