// Package domain defines the harness's core, transport- and OS-agnostic
// concepts: Node, Resource, Capability, Command, Event, and the Transport
// interface. Nothing in this package may import a concrete transport,
// runtime, or OS-specific package.
package domain

// NodeID is a persistent node identity, derived from a public key. It must
// remain stable across IP changes, reconnects, and manager restarts.
type NodeID string

// ManagerNodeID is the well-known logical address of the harness manager,
// used as a message destination.
const ManagerNodeID NodeID = "manager"

// NodeState is a node's position in the connection lifecycle
// (see Home_Compute_Harness_Baseline_Architecture.md §4 / v1.md §6).
type NodeState string

const (
	NodeUnknown      NodeState = "UNKNOWN"
	NodeDiscovered   NodeState = "DISCOVERED"
	NodeRegistering  NodeState = "REGISTERING"
	NodeConnected    NodeState = "CONNECTED"
	NodeReady        NodeState = "READY"
	NodeDegraded     NodeState = "DEGRADED"
	NodeOffline      NodeState = "OFFLINE"
	NodeReconnecting NodeState = "RECONNECTING"
)

// Identity is the cryptographic identity of a node: a persistent NodeID
// derived from an Ed25519 public key, plus the key itself for verification.
type Identity struct {
	NodeID    NodeID `json:"nodeId"`
	PublicKey []byte `json:"publicKey"`
}

// Platform describes the OS/architecture a node runs on. It is metadata,
// not a branch point for core logic.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// Node is a participant in the fabric: its stable identity and metadata.
// It deliberately excludes fast-changing runtime state (see RuntimeState).
type Node struct {
	Identity     Identity `json:"identity"`
	Hostname     string   `json:"hostname"`
	Name         string   `json:"name"`
	Platform     Platform `json:"platform"`
	AgentVersion string   `json:"agentVersion"`
	// BinaryHash is the SHA-256 (hex) of the agent's own currently-running
	// executable, computed once at startup — unlike AgentVersion (a
	// hardcoded string nothing ever overrides today), this is the actual
	// signal the manager uses to decide whether a node needs a self-update
	// (see internal/manager's NeedsUpdate): a content hash needs no human
	// to remember to bump a version number.
	BinaryHash string `json:"binaryHash,omitempty"`
	// AppHash is the SHA-256 (hex) of the installed app this agent ships
	// inside (the Android app's APK), for an agent that is updated with
	// its app (FeatureAppUpdate). Empty otherwise.
	AppHash string `json:"appHash,omitempty"`
	// HostFingerprint is a salted hash of the machine's own ID (see
	// internal/sysinfo.HostFingerprint), letting the manager notice two
	// identities on one machine. Agent-asserted, so only ever a hint.
	// HostFingerprintSource is "machine" (persistent) or "boot" (changes
	// every reboot, e.g. under Termux). Both empty for older agents.
	HostFingerprint       string `json:"hostFingerprint,omitempty"`
	HostFingerprintSource string `json:"hostFingerprintSource,omitempty"`
}
