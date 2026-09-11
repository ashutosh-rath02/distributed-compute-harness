// Package protocol defines the Harness Protocol wire format: a versioned
// message envelope carrying typed payloads, independent of whatever
// transport carries the bytes (v1.md §8).
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"home-harness/internal/domain"
)

// Version is the protocol version this build speaks. It follows "vMAJOR.MINOR";
// messages are accepted when their major version matches ours (see CheckVersion).
const Version = "v0.1"

// MessageType enumerates the Harness Protocol message types for v0 (v1.md §8).
type MessageType string

const (
	MsgDiscover         MessageType = "DISCOVER"
	MsgRegister         MessageType = "REGISTER"
	MsgRegisterAck      MessageType = "REGISTER_ACK"
	MsgRegisterReject   MessageType = "REGISTER_REJECT"
	MsgHeartbeat        MessageType = "HEARTBEAT"
	MsgStateUpdate      MessageType = "STATE_UPDATE"
	MsgCapabilityUpdate MessageType = "CAPABILITY_UPDATE"
	MsgCommand          MessageType = "COMMAND"
	MsgCommandResult    MessageType = "COMMAND_RESULT"
	MsgPing             MessageType = "PING"
	MsgPong             MessageType = "PONG"
	MsgError            MessageType = "ERROR"
)

// Envelope is the outer frame for every Harness Protocol message.
type Envelope struct {
	MessageID       string          `json:"messageId"`
	ProtocolVersion string          `json:"protocolVersion"`
	Type            MessageType     `json:"type"`
	Source          domain.NodeID   `json:"source"`
	Destination     domain.NodeID   `json:"destination"`
	Timestamp       time.Time       `json:"timestamp"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope builds an envelope with a fresh message ID, the current
// protocol version, and the payload marshaled to JSON.
func NewEnvelope(msgType MessageType, source, destination domain.NodeID, payload any) (*Envelope, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("protocol: marshal payload: %w", err)
		}
		raw = b
	}
	id, err := newMessageID()
	if err != nil {
		return nil, err
	}
	return &Envelope{
		MessageID:       id,
		ProtocolVersion: Version,
		Type:            msgType,
		Source:          source,
		Destination:     destination,
		Timestamp:       time.Now().UTC(),
		Payload:         raw,
	}, nil
}

// DecodePayload unmarshals the envelope's payload into v.
func (e *Envelope) DecodePayload(v any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("protocol: envelope %s has no payload", e.MessageID)
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("protocol: decode payload for %s: %w", e.Type, err)
	}
	return nil
}

// Encode serializes an envelope to its wire form.
func Encode(e *Envelope) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode envelope: %w", err)
	}
	return b, nil
}

// Decode parses an envelope from its wire form.
func Decode(data []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("protocol: decode envelope: %w", err)
	}
	return &e, nil
}

// ErrUnsupportedVersion indicates an envelope's protocol version is
// incompatible with this build.
type ErrUnsupportedVersion struct {
	Got, Want string
}

func (e *ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("protocol: unsupported version %q (this build speaks %q)", e.Got, e.Want)
}

// CheckVersion accepts envelopes whose major version matches ours (e.g.
// "v0.3" is accepted by a "v0.1" build; "v1.0" is not). This lets minor
// protocol additions land without breaking older peers, per v1.md §8/§21.
func CheckVersion(e *Envelope) error {
	if Major(e.ProtocolVersion) != Major(Version) {
		return &ErrUnsupportedVersion{Got: e.ProtocolVersion, Want: Version}
	}
	return nil
}

// Major extracts the major component of a "vMAJOR.MINOR" version string,
// e.g. "v0.3" -> "0". Exported so other packages that need to reason about
// version compatibility (e.g. UDP discovery, which filters beacons before
// a full envelope even exists) share this exact rule instead of each
// re-implementing it.
func Major(v string) string {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 2)
	return parts[0]
}

func newMessageID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("protocol: generate message id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
