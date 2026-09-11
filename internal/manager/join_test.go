package manager

import "testing"

func TestJoinInfoSecureWithAgentBinaryConfigured(t *testing.T) {
	s := &Server{
		cfg:             Config{Fingerprint: "abc123", PairingToken: "shhh"},
		agentBinaryHash: "deadbeef",
	}
	info := s.JoinInfo()
	if info.Fingerprint != "abc123" {
		t.Errorf("Fingerprint = %q, want %q", info.Fingerprint, "abc123")
	}
	if info.PairingToken != "shhh" {
		t.Errorf("PairingToken = %q, want %q", info.PairingToken, "shhh")
	}
	if info.Insecure {
		t.Error("expected Insecure false when a Fingerprint is configured")
	}
	if !info.AgentBinaryAvailable {
		t.Error("expected AgentBinaryAvailable true when agentBinaryHash is set")
	}
	if info.AgentBinarySHA256 != "deadbeef" {
		t.Errorf("AgentBinarySHA256 = %q, want %q", info.AgentBinarySHA256, "deadbeef")
	}
}

func TestJoinInfoInsecureWhenFingerprintEmpty(t *testing.T) {
	s := &Server{cfg: Config{PairingToken: "shhh"}}
	info := s.JoinInfo()
	if !info.Insecure {
		t.Error("expected Insecure true when Fingerprint is empty")
	}
}

func TestJoinInfoAgentBinaryUnavailableWhenNotConfigured(t *testing.T) {
	s := &Server{cfg: Config{Fingerprint: "abc123", PairingToken: "shhh"}}
	info := s.JoinInfo()
	if info.AgentBinaryAvailable {
		t.Error("expected AgentBinaryAvailable false when agentBinaryHash is empty")
	}
}
