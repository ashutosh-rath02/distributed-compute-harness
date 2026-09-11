package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	"home-harness/internal/transport/ws"
)

func TestRegisterOverTLSWithPinnedFingerprint(t *testing.T) {
	const addr = "127.0.0.1:19250"

	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := manager.NewServer(ws.NewTLSServer(cert), nil, manager.Config{
		Addr:             addr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})
	go srv.Run(ctx)

	a, err := agent.New(ws.NewTLSClient(mtls.PinnedClientConfig(mtls.Fingerprint(cert))), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "tls-agent"),
		Name:              "tls-agent",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
}

func TestAgentRejectsRogueManagerWithWrongFingerprint(t *testing.T) {
	const addr = "127.0.0.1:19251"

	realCert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert (real): %v", err)
	}
	// A different cert from a different dir simulates a rogue process
	// answering at the expected address with its own, unrelated identity.
	rogueCert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert (rogue): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The manager actually running is the "rogue" one — the agent is
	// configured to trust the "real" one's fingerprint instead.
	srv := manager.NewServer(ws.NewTLSServer(rogueCert), nil, manager.Config{
		Addr:             addr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})
	go srv.Run(ctx)

	a, err := agent.New(ws.NewTLSClient(mtls.PinnedClientConfig(mtls.Fingerprint(realCert))), agent.Config{
		ManagerAddr:       addr,
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "pinned-agent"),
		HeartbeatInterval: 100 * time.Millisecond,
		ReconnectBackoff:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)

	// Give it several reconnect/reject cycles; it must never register
	// against the rogue manager.
	time.Sleep(300 * time.Millisecond)
	if _, ok := srv.Registry.Get(a.NodeID()); ok {
		t.Fatal("expected agent to never register with a manager presenting the wrong TLS fingerprint")
	}
}
