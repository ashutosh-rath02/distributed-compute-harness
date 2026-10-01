package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

type joinInfoView struct {
	Fingerprint          string `json:"fingerprint"`
	PairingToken         string `json:"pairingToken"`
	Insecure             bool   `json:"insecure"`
	AgentBinaryAvailable bool   `json:"agentBinaryAvailable"`
	AgentBinarySHA256    string `json:"agentBinarySha256"`
	RelayAvailable       bool   `json:"relayAvailable"`
	RelayAddr            string `json:"relayAddr"`
	RelayToken           string `json:"relayToken"`
}

// TestJoinInfoEndpointReturnsConfiguredValues proves GET /join-info (the
// mechanism cmd/harnessctl's `join` command relies on, v5 part 1) reflects
// the manager's real configuration over the real HTTP API — not just the
// underlying JoinInfo() method in isolation.
func TestJoinInfoEndpointReturnsConfiguredValues(t *testing.T) {
	const addr = "127.0.0.1:19298"
	binaryPath, wantHash := writeDummyAgentBinary(t, []byte("content"))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr:             addr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
		AgentBinaryPath:  binaryPath,
		Fingerprint:      "test-fingerprint",
		RelayAddr:        "relay.example.com:8420",
		RelayToken:       "relay-secret",
	})
	transport.Handle("/agent-binary", srv.AgentBinaryHandler())
	go func() {
		if err := srv.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("manager exited: %v", err)
		}
	}()

	apiSrv := httptest.NewServer(srv.NewHTTPHandler())
	defer apiSrv.Close()

	var info joinInfoView
	waitForJoinInfo(t, apiSrv.URL, &info)

	if info.Fingerprint != "test-fingerprint" {
		t.Errorf("Fingerprint = %q, want %q", info.Fingerprint, "test-fingerprint")
	}
	if info.PairingToken != pairingToken {
		t.Errorf("PairingToken = %q, want %q", info.PairingToken, pairingToken)
	}
	if info.Insecure {
		t.Error("expected Insecure false when Fingerprint is configured")
	}
	if !info.AgentBinaryAvailable {
		t.Error("expected AgentBinaryAvailable true when -agent-binary is configured")
	}
	if info.AgentBinarySHA256 != wantHash {
		t.Errorf("AgentBinarySHA256 = %q, want %q", info.AgentBinarySHA256, wantHash)
	}
	if !info.RelayAvailable || info.RelayAddr != "relay.example.com:8420" || info.RelayToken != "relay-secret" {
		t.Errorf("unexpected relay join info: available=%v addr=%q token=%q", info.RelayAvailable, info.RelayAddr, info.RelayToken)
	}
}

// TestJoinInfoInsecureWithoutAgentBinary proves the two independent
// "degraded" signals (no TLS fingerprint, no agent binary configured)
// surface correctly and independently over the real API.
func TestJoinInfoInsecureWithoutAgentBinary(t *testing.T) {
	const addr = "127.0.0.1:19299"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr:             addr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})
	go func() {
		if err := srv.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("manager exited: %v", err)
		}
	}()

	apiSrv := httptest.NewServer(srv.NewHTTPHandler())
	defer apiSrv.Close()

	var info joinInfoView
	waitForJoinInfo(t, apiSrv.URL, &info)

	if !info.Insecure {
		t.Error("expected Insecure true when no Fingerprint is configured")
	}
	if info.AgentBinaryAvailable {
		t.Error("expected AgentBinaryAvailable false when -agent-binary is unset")
	}
	if info.RelayAvailable || info.RelayAddr != "" || info.RelayToken != "" {
		t.Errorf("expected relay details to be absent when relay is disabled, got %+v", info)
	}
}

func waitForJoinInfo(t *testing.T, apiBase string, into *joinInfoView) {
	t.Helper()
	resp, err := http.Get(apiBase + "/join-info")
	if err != nil {
		t.Fatalf("GET /join-info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /join-info: expected 200, got %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode /join-info: %v", err)
	}
}
