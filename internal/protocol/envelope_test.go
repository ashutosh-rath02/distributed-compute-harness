package protocol

import (
	"testing"
	"time"

	"home-harness/internal/domain"
)

func TestNewEnvelopeRoundTrip(t *testing.T) {
	payload := HeartbeatPayload{
		RuntimeState: domain.RuntimeState{
			NodeID:     "node-abc123",
			State:      domain.NodeReady,
			CPUPercent: 12.5,
		},
	}

	env, err := NewEnvelope(MsgHeartbeat, "node-abc123", domain.ManagerNodeID, payload)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.MessageID == "" {
		t.Fatal("expected non-empty message id")
	}
	if env.ProtocolVersion != Version {
		t.Fatalf("expected protocol version %q, got %q", Version, env.ProtocolVersion)
	}

	wire, err := Encode(env)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	decoded, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Type != MsgHeartbeat {
		t.Fatalf("expected type %q, got %q", MsgHeartbeat, decoded.Type)
	}
	if decoded.Source != "node-abc123" || decoded.Destination != domain.ManagerNodeID {
		t.Fatalf("source/destination not preserved: got source=%q destination=%q", decoded.Source, decoded.Destination)
	}

	var gotPayload HeartbeatPayload
	if err := decoded.DecodePayload(&gotPayload); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if gotPayload.RuntimeState.NodeID != payload.RuntimeState.NodeID {
		t.Fatalf("payload not preserved: got %+v, want %+v", gotPayload, payload)
	}
	if gotPayload.RuntimeState.State != domain.NodeReady {
		t.Fatalf("expected state %q, got %q", domain.NodeReady, gotPayload.RuntimeState.State)
	}
}

func TestNewEnvelopeUniqueMessageIDs(t *testing.T) {
	a, err := NewEnvelope(MsgPing, "node-a", domain.ManagerNodeID, nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	b, err := NewEnvelope(MsgPing, "node-a", domain.ManagerNodeID, nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if a.MessageID == b.MessageID {
		t.Fatal("expected distinct message ids across calls")
	}
}

func TestCheckVersionAcceptsMatchingMajor(t *testing.T) {
	env := &Envelope{ProtocolVersion: "v0.9"}
	if err := CheckVersion(env); err != nil {
		t.Fatalf("expected same-major version to be accepted, got: %v", err)
	}
}

func TestCheckVersionRejectsDifferentMajor(t *testing.T) {
	env := &Envelope{ProtocolVersion: "v1.0"}
	err := CheckVersion(env)
	if err == nil {
		t.Fatal("expected different-major version to be rejected")
	}
	var verErr *ErrUnsupportedVersion
	if _, ok := err.(*ErrUnsupportedVersion); !ok {
		t.Fatalf("expected *ErrUnsupportedVersion, got %T: %v (%v)", err, err, verErr)
	}
}

func TestDecodePayloadWithoutPayloadErrors(t *testing.T) {
	env, err := NewEnvelope(MsgPing, "node-a", domain.ManagerNodeID, nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	var v struct{}
	if err := env.DecodePayload(&v); err == nil {
		t.Fatal("expected error decoding empty payload")
	}
}

func TestEncodeDecodePreservesTimestamp(t *testing.T) {
	env, err := NewEnvelope(MsgPong, "node-a", "node-b", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	wire, err := Encode(env)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !decoded.Timestamp.Truncate(time.Second).Equal(env.Timestamp.Truncate(time.Second)) {
		t.Fatalf("timestamp not preserved: got %v, want %v", decoded.Timestamp, env.Timestamp)
	}
}
