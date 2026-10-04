package manager

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/failover"
	"home-harness/internal/mtls"
	"home-harness/internal/protocol"
)

func fencingServer(t *testing.T, term uint64) (*Server, chan [3]uint64) {
	t.Helper()
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan [3]uint64, 2)
	s := NewServer(nil, nil, Config{TLSCert: cert, Fingerprint: mtls.Fingerprint(cert), StepDown: func(p domain.StandbyPair, own, newTerm uint64) {
		calls <- [3]uint64{own, newTerm, uint64(len(p.PeerAddr))}
	}})
	s.failover.term = term
	s.failover.pair = &domain.StandbyPair{Secret: "s", PeerAddr: "10.0.0.2:7420"}
	return s, calls
}

func rejectOf(t *testing.T, conn *recordingConn) protocol.RegisterRejectPayload {
	t.Helper()
	select {
	case data := <-conn.sent:
		env, _ := protocol.Decode(data)
		var p protocol.RegisterRejectPayload
		if env.Type != protocol.MsgRegisterReject || env.DecodePayload(&p) != nil {
			t.Fatalf("sent %s", env.Type)
		}
		return p
	default:
		t.Fatal("nothing sent")
	}
	return protocol.RegisterRejectPayload{}
}

// The REGISTER fence: only a provably newer term makes the manager step
// down (and turn the device away); afterwards every device is refused.
func TestRegisterFence(t *testing.T) {
	s, calls := fencingServer(t, 2)
	ctx := context.Background()
	conn := &recordingConn{sent: make(chan []byte, 4)}
	other, _ := mtls.LoadOrCreateCert(t.TempDir())
	foreign, _ := failover.SignTerm(other, 3)
	equal, _ := failover.SignTerm(s.cfg.TLSCert, 2)
	for name, c := range map[string]struct {
		term  uint64
		proof []byte
	}{"none": {0, nil}, "lower": {1, equal}, "equal": {2, equal}, "garbage": {3, []byte("x")}, "foreign key": {3, foreign}} {
		if s.refuseIfSuperseded(ctx, conn, "n", c.term, c.proof) || s.SteppedDown() {
			t.Fatalf("%s: fenced", name)
		}
	}
	valid, _ := failover.SignTerm(s.cfg.TLSCert, 3)
	if !s.refuseIfSuperseded(ctx, conn, "n", 3, valid) || !s.SteppedDown() {
		t.Fatal("a valid higher term didn't fence")
	}
	if p := rejectOf(t, conn); !p.NotActive {
		t.Fatalf("reject: %+v", p)
	}
	select {
	case c := <-calls:
		if c[0] != 2 || c[1] != 3 || c[2] == 0 {
			t.Fatalf("StepDown(%v)", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StepDown wasn't called")
	}
	// Fenced: a device with no term at all is turned away too, and
	// StepDown isn't called twice.
	if !s.refuseIfSuperseded(ctx, conn, "n", 0, nil) {
		t.Fatal("a stepped-down manager admitted a device")
	}
	rejectOf(t, conn)
	select {
	case c := <-calls:
		t.Fatalf("StepDown called again: %v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

// Without TLS there is no key to verify with: nothing can fence.
func TestRegisterFenceNeedsTLS(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	conn := &recordingConn{sent: make(chan []byte, 1)}
	if s.refuseIfSuperseded(context.Background(), conn, "n", 5, []byte("x")) {
		t.Fatal("fenced without an identity")
	}
}

func TestRegisterAckOnlyForFailoverAgents(t *testing.T) {
	s, _ := fencingServer(t, 2)
	s.cfg.AdvertiseAddr = "10.0.0.1:7420"
	s.failover.proof = []byte("p")
	ack := s.registerAck("n", domain.Manifest{})
	if ack.Term != 0 || ack.Managers != nil {
		t.Fatalf("an agent without failover.v1 got %+v", ack)
	}
	ack = s.registerAck("n", domain.Manifest{AgentFeatures: []string{domain.FeatureFailover}})
	if ack.Term != 2 || string(ack.TermProof) != "p" || strings.Join(ack.Managers, ",") != "10.0.0.1:7420,10.0.0.2:7420" {
		t.Fatalf("failover agent got %+v", ack)
	}
}

// An expired enrollment token is refused like a wrong one.
func TestStandbyEnrollmentExpires(t *testing.T) {
	s, _ := fencingServer(t, 0)
	s.failover.enrollHash = sha256.Sum256([]byte("tok"))
	s.failover.enrollUntil = time.Now().Add(-time.Second)
	req := httptest.NewRequest(http.MethodPost, failover.RouteEnroll, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	s.standbyEnroll(w, req, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", w.Code)
	}
}

type fakeStandby struct{ promoted bool }

func (f *fakeStandby) Status() failover.ReplicaStatus { return failover.ReplicaStatus{Role: "standby"} }
func (f *fakeStandby) Promote(ctx context.Context, force bool, actor string) error {
	f.promoted = true
	return nil
}

// The standby's operator API needs the operator token like the full one.
func TestStandbyRoleAPIRequiresOperator(t *testing.T) {
	fs := &fakeStandby{}
	srv := httptest.NewServer(StandbyRoleHandler(fs, strings.Repeat("t", 64)))
	defer srv.Close()
	for _, c := range []struct{ method, path string }{{"GET", "/standby"}, {"POST", "/standby/promote"}} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s without the token: %d", c.method, c.path, resp.StatusCode)
		}
	}
	if fs.promoted {
		t.Fatal("promoted without the operator token")
	}
	req, _ := http.NewRequest("POST", srv.URL+"/standby/promote", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !fs.promoted {
		t.Fatalf("promote with the token: %d", resp.StatusCode)
	}
}
