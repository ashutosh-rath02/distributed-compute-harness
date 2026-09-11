package domain

// CapabilityName is an open string type, extensible by adapters (camera,
// Bluetooth, AI inference, ...) without changing the core domain.
type CapabilityName string

const (
	CapabilitySystemExecute   CapabilityName = "system.execute"
	CapabilityFilesystemRead  CapabilityName = "filesystem.read"
	CapabilityFilesystemWrite CapabilityName = "filesystem.write"
)

// Capability is an action or function a node claims to support. In v0,
// capabilities are advertised in the manifest for future use; only the
// commands in the CommandName set (see command.go) are actually invocable.
type Capability struct {
	Name    CapabilityName `json:"name"`
	Version string         `json:"version"`
}
