package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

// An agent accepts a manager only at a term at least the one it has seen
// (for that manager identity), and keeps the highest.
func TestAckTermNeverGoesBack(t *testing.T) {
	a := newPairingAgent(t, Config{ManagerFingerprint: "fp1", Failover: true})
	if err := a.noteAck("m1", protocol.RegisterAckPayload{Term: 2, TermProof: []byte("p2"), Managers: []string{"10.0.0.1:7420", "10.0.0.2:7420"}}); err != nil {
		t.Fatal(err)
	}
	err := a.noteAck("m0", protocol.RegisterAckPayload{Term: 1})
	if !errors.Is(err, errBehind) {
		t.Fatalf("a lower term was accepted: %v", err)
	}
	if err := a.noteAck("m0", protocol.RegisterAckPayload{}); !errors.Is(err, errBehind) {
		t.Fatalf("a manager without terms was accepted after term 2: %v", err)
	}
	if err := a.noteAck("m1", protocol.RegisterAckPayload{Term: 2}); err != nil {
		t.Fatalf("the same term was refused: %v", err)
	}
	k := a.knownManagers()
	if k.Term != 2 || string(k.TermProof) != "p2" || strings.Join(k.Managers, ",") != "10.0.0.1:7420,10.0.0.2:7420" {
		t.Fatalf("known: %+v", k)
	}
	// Kept on disk: a restarted agent still knows.
	b, _ := New(a.transport, a.cfg)
	if b.knownManagers().Term != 2 {
		t.Fatal("the term didn't survive a restart")
	}
}

// What an agent learned belongs to the manager identity it pins: after
// re-pairing with another manager it starts afresh rather than refusing
// every answer.
func TestKnownManagersFollowThePin(t *testing.T) {
	a := newPairingAgent(t, Config{ManagerFingerprint: "fp1", Failover: true})
	a.remember(5, []byte("p"), []string{"10.0.0.1:7420"})
	a.cfg.ManagerFingerprint = "fp2"
	if k := a.knownManagers(); k.Term != 0 || len(k.Managers) != 0 {
		t.Fatalf("another identity's term applies: %+v", k)
	}
	if err := a.noteAck("m", protocol.RegisterAckPayload{Term: 0}); err != nil {
		t.Fatalf("the new manager was refused: %v", err)
	}
}

func TestManagerCandidates(t *testing.T) {
	a := newPairingAgent(t, Config{ManagerFingerprint: "fp", Failover: true})
	a.remember(1, nil, []string{"10.0.0.2:7420", "10.0.0.1:7420", "not an address", "10.0.0.2:7420"})
	if got := strings.Join(a.managerCandidates("10.0.0.1:7420"), ","); got != "10.0.0.1:7420,10.0.0.2:7420" {
		t.Fatalf("candidates: %s", got)
	}
	if got := strings.Join(a.managerCandidates(""), ","); got != "10.0.0.2:7420,10.0.0.1:7420" {
		t.Fatalf("candidates without a resolved address: %s", got)
	}
	a.cfg.Failover = false
	if got := strings.Join(a.managerCandidates("10.0.0.1:7420"), ","); got != "10.0.0.1:7420" {
		t.Fatalf("an agent without failover tries %s", got)
	}
}

// failover.v1 is advertised only when on.
func TestFailoverFeature(t *testing.T) {
	has := func(a *Agent) bool {
		for _, f := range a.agentFeatures() {
			if f == domain.FeatureFailover {
				return true
			}
		}
		return false
	}
	if !has(newPairingAgent(t, Config{Failover: true})) || has(newPairingAgent(t, Config{})) {
		t.Fatal("failover.v1 advertised wrongly")
	}
}

type silentConn struct{ sends chan []byte }

func (c *silentConn) Send(ctx context.Context, data []byte) error {
	c.sends <- data
	return nil
}
func (c *silentConn) Receive(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *silentConn) RemoteAddr() string { return "silent" }
func (c *silentConn) Close() error       { return nil }

// A manager that goes silent without closing the connection (its PC
// slept) is given up on: pinged every heartbeat, dropped once nothing has
// come back for the limit.
func TestWatchManagerGivesUpOnSilence(t *testing.T) {
	a := newPairingAgent(t, Config{Failover: true, HeartbeatInterval: 50 * time.Millisecond})
	conn := &silentConn{sends: make(chan []byte, 1000)}
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	go a.watchManager(ctx, conn, errCh)
	select {
	case err := <-errCh:
		if took := time.Since(start); took < 9*time.Second || took > 15*time.Second {
			t.Fatalf("gave up after %s, want ~10s", took)
		}
		if !strings.Contains(err.Error(), "stopped answering") {
			t.Fatalf("err: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("never gave up on a silent manager")
	}
	env, _ := protocol.Decode(<-conn.sends)
	if env.Type != protocol.MsgPing {
		t.Fatalf("sent %s, want PING", env.Type)
	}
}
