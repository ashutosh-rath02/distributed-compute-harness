package protocol

import (
	"testing"
)

// FuzzDecodeEnvelope: whatever bytes arrive on a connection, decoding the
// envelope and then every payload type must not panic, and an envelope that
// decodes must encode again.
func FuzzDecodeEnvelope(f *testing.F) {
	f.Add([]byte(`{"protocolVersion":"1.0","type":"REGISTER","messageId":"m1","source":"n1","destination":"manager","payload":{"manifest":{"name":"x"},"pairingToken":"t","signature":"AAAA"}}`))
	f.Add([]byte(`{"protocolVersion":"1.0","type":"WORKLOAD_ASSIGN","payload":{"workload":{"id":"w","command":"echo","args":["a"]}}}`))
	f.Add([]byte(`{"type":"COMMAND_RESULT","payload":[1,2,3]}`))
	f.Add([]byte(`{"payload":null}`))
	f.Add([]byte(`[`))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := Decode(data)
		if err != nil {
			return
		}
		_ = CheckVersion(e)
		_ = Major(e.ProtocolVersion)
		for _, v := range []any{
			new(RegisterPayload), new(RegisterAckPayload), new(ManagersPayload), new(RegisterRejectPayload),
			new(HeartbeatPayload), new(StateUpdatePayload), new(CapabilityUpdatePayload), new(CommandPayload),
			new(CommandResultPayload), new(WorkloadAssignPayload), new(TunnelOpenPayload), new(WorkloadStatusPayload),
			new(WorkloadCancelPayload), new(ErrorPayload),
		} {
			_ = e.DecodePayload(v)
		}
		if _, err := Encode(e); err != nil {
			// A payload that was valid JSON on the way in must be on the way out.
			t.Fatalf("decoded envelope does not encode: %v", err)
		}
	})
}

// FuzzPairingCode: the code shown to a human is always "NNN NNN".
func FuzzPairingCode(f *testing.F) {
	f.Add("AB:CD", []byte{1, 2, 3})
	f.Add("", []byte(nil))
	f.Fuzz(func(t *testing.T, fp string, key []byte) {
		c := PairingCode(fp, key)
		if len(c) != 7 || c[3] != ' ' {
			t.Fatalf("code %q", c)
		}
	})
}
