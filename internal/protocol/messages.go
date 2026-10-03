package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
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
	// Pairing asks to be admitted by the operator's approval instead of a
	// token (agent -pair): an unknown identity is held as a join request
	// until someone approves it on the manager, comparing PairingCode on
	// both screens.
	Pairing bool `json:"pairing,omitempty"`
}

// PairingCode is the short code a device and its manager both show while
// a join request waits for approval: derived from the manager's TLS
// fingerprint and the device's public key, computed by each side on its
// own. A machine in the middle (its own certificate toward the device,
// its own key toward the manager) makes the two codes differ.
func PairingCode(managerFingerprint string, publicKey []byte) string {
	h := sha256.New()
	h.Write([]byte("home-harness-pair-v1:" + strings.ToLower(strings.TrimSpace(managerFingerprint)) + ":"))
	h.Write(publicKey)
	sum := h.Sum(nil)
	n := binary.BigEndian.Uint64(sum[:8]) % 1_000_000
	return fmt.Sprintf("%03d %03d", n/1000, n%1000)
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
	// PendingApproval: not refused, waiting for the operator to approve
	// this device (Code is the manager's PairingCode). The agent retries
	// shortly; nothing about this is an error.
	PendingApproval bool   `json:"pendingApproval,omitempty"`
	Code            string `json:"code,omitempty"`
	// JoinClosed: the manager isn't taking new devices (its join window
	// is closed). The agent asks again later, quietly.
	JoinClosed bool `json:"joinClosed,omitempty"`
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
	// ArtifactToken authorizes this one assignment's file transfers (only
	// set when the workload declares files). It is deliberately not part
	// of domain.Workload, so it is never persisted or shown by the API.
	ArtifactToken string `json:"artifactToken,omitempty"`
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
