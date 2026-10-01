package protocol

import (
	"time"

	"home-harness/internal/domain"
)

// RegisterPayload is sent by an agent to request admission to the fabric.
// PairingToken is the first-admission credential from v1.md §4.2. It may
// be the operator's shared token or a short-lived one-time enrollment token;
// after admission the persistent signing key authenticates reconnects.
// Network presence alone never grants execution authority
// (baseline §5). Signature proves possession of the private key behind
// Manifest.Node.Identity.PublicKey: it is an Ed25519 signature, made with
// that key, over RegisterSignedData(PairingToken, NodeID). Without it, a
// node could claim any public key/NodeID pair it likes as long as it knew
// the shared pairing token — the signature is what actually makes the
// identity unspoofable, not just the NodeID's derivation from the key.
type RegisterPayload struct {
	Manifest     domain.Manifest `json:"manifest"`
	PairingToken string          `json:"pairingToken"`
	Signature    []byte          `json:"signature"`
}

// RegisterSignedData builds the exact byte sequence a REGISTER's Signature
// must cover, shared by both the agent (which signs it) and the manager
// (which verifies it) so the two never drift apart.
func RegisterSignedData(pairingToken string, nodeID domain.NodeID) []byte {
	return []byte(pairingToken + ":" + string(nodeID))
}

// RegisterAckPayload confirms admission and returns the manager's view of
// the node's (already-known) identity.
type RegisterAckPayload struct {
	NodeID     domain.NodeID `json:"nodeId"`
	ServerTime time.Time     `json:"serverTime"`
}

// RegisterRejectPayload explains why REGISTER was refused (e.g. bad
// pairing token), so rejection is observable rather than a silent hang.
type RegisterRejectPayload struct {
	Reason string `json:"reason"`
}

// HeartbeatPayload carries the sending node's current runtime state.
type HeartbeatPayload struct {
	RuntimeState domain.RuntimeState `json:"runtimeState"`
}

// StateUpdatePayload is an out-of-band runtime state push (distinct from a
// regular heartbeat tick), e.g. after a manual "refresh state" request.
type StateUpdatePayload struct {
	RuntimeState domain.RuntimeState `json:"runtimeState"`
}

// CapabilityUpdatePayload republishes a node's resources/capabilities,
// e.g. after REQUEST_RESOURCE_REFRESH.
type CapabilityUpdatePayload struct {
	Resources    []domain.Resource   `json:"resources"`
	Capabilities []domain.Capability `json:"capabilities"`
}

// CommandPayload wraps a Command dispatched to a node.
type CommandPayload struct {
	Command domain.Command `json:"command"`
}

// CommandResultPayload wraps a node's structured reply to a Command.
type CommandResultPayload struct {
	Result domain.CommandResult `json:"result"`
}

// WorkloadAssignPayload dispatches a workload for execution to a node.
type WorkloadAssignPayload struct {
	Workload domain.Workload `json:"workload"`
}

// WorkloadStatusPayload reports a workload's current or final status. Sent
// by the agent on state transitions (started, completed, failed, canceled)
// and stored by the manager against the workload's record.
type WorkloadStatusPayload struct {
	Status domain.WorkloadStatus `json:"status"`
}

// WorkloadCancelPayload requests that a running workload be terminated.
type WorkloadCancelPayload struct {
	ID domain.WorkloadID `json:"id"`
}

// ErrorPayload is a generic protocol-level error report.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
