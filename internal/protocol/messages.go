package protocol

import (
	"time"

	"home-harness/internal/domain"
)

// RegisterPayload is sent by an agent to request admission to the fabric.
// PairingToken is the minimum viable "identity credential" from v1.md
// §4.2 — network presence alone never grants execution authority
// (baseline §5).
type RegisterPayload struct {
	Manifest     domain.Manifest `json:"manifest"`
	PairingToken string          `json:"pairingToken"`
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

// ErrorPayload is a generic protocol-level error report.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
