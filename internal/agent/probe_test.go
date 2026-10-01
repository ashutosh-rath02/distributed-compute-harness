package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

// The capability probe belongs to one connection: reconnecting must not
// leave the previous connection's probe running (on a phone manager's
// flaky Wi-Fi they would pile up, each polling the model runtime).
func TestCapabilityProbeEndsWithItsConnection(t *testing.T) {
	const addr = "127.0.0.1:19590"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conns, err := ws.New().Listen(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 16)
	// A stand-in manager: admit every REGISTER, then hang up shortly
	// after, forcing a reconnect.
	go func() {
		for conn := range conns {
			data, err := conn.Receive(ctx)
			if err != nil {
				conn.Close()
				continue
			}
			env, _ := protocol.Decode(data)
			ack, _ := protocol.NewEnvelope(protocol.MsgRegisterAck, domain.ManagerNodeID, env.Source, protocol.RegisterAckPayload{NodeID: env.Source, ServerTime: time.Now()})
			wire, _ := protocol.Encode(ack)
			conn.Send(ctx, wire)
			accepted <- struct{}{}
			time.Sleep(150 * time.Millisecond)
			go conn.Close()
		}
	}()
	a, err := New(ws.New(), Config{
		ManagerAddr: addr, PairingToken: "t", IdentityDir: filepath.Join(t.TempDir(), "id"), WorkDir: filepath.Join(t.TempDir(), "work"),
		HeartbeatInterval: 50 * time.Millisecond, ReconnectBackoff: 20 * time.Millisecond, MaxReconnectBackoff: 50 * time.Millisecond,
		HostFingerprint: "-", OllamaURL: "127.0.0.1:1", CapabilityProbeInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	for i := 0; i < 5; i++ {
		select {
		case <-accepted:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d connections", i)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := a.probeLoops.Load(); n > 1 {
		t.Fatalf("%d capability probes running after 5 connections, want at most 1", n)
	}
}
