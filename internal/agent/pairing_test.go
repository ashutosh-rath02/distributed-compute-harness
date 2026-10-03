package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

type failingDiscoverer struct{ calls int }

func (d *failingDiscoverer) Discover(context.Context) (string, error) {
	d.calls++
	return "", errors.New("no beacon")
}

func newPairingAgent(t *testing.T, cfg Config) *Agent {
	t.Helper()
	dir := t.TempDir()
	cfg.IdentityDir = filepath.Join(dir, "id")
	cfg.WorkDir = filepath.Join(dir, "work")
	cfg.HostFingerprint = "-"
	a, err := New(ws.New(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// A network that drops multicast must not leave a device unable to find
// the manager it was installed from.
func TestResolveFallsBackWhenDiscoveryFindsNothing(t *testing.T) {
	d := &failingDiscoverer{}
	a := newPairingAgent(t, Config{Discoverer: d, ManagerAddrFallback: "192.168.1.20:7420"})
	addr, err := a.resolveManagerAddr(context.Background())
	if err != nil || addr != "192.168.1.20:7420" || d.calls != 1 {
		t.Fatalf("got %q, %v after %d discoveries", addr, err, d.calls)
	}
	if _, err := newPairingAgent(t, Config{Discoverer: d}).resolveManagerAddr(context.Background()); err == nil {
		t.Fatal("without a fallback, a failed discovery must stay an error")
	}
}

// While waiting for approval the agent asks again every couple of
// seconds: straight back to the manager that answered, not through
// another discovery timeout each time.
func TestPendingRetryGoesStraightBack(t *testing.T) {
	d := &failingDiscoverer{}
	a := newPairingAgent(t, Config{Discoverer: d, Pairing: true})
	a.pendingAddr = "10.0.0.7:7420"
	addr, err := a.resolveManagerAddr(context.Background())
	if err != nil || addr != "10.0.0.7:7420" || d.calls != 0 {
		t.Fatalf("got %q, %v after %d discoveries", addr, err, d.calls)
	}
}

func TestPairingCodeUsesThePinnedCertificate(t *testing.T) {
	seen := "ab"
	a := newPairingAgent(t, Config{Pairing: true, ManagerFingerprintFunc: func() string { return seen }})
	if got, want := a.PairingCode(), protocol.PairingCode(seen, a.identity.PublicKey); got != want {
		t.Fatalf("code %s, want %s", got, want)
	}
	seen = "cd"
	if a.PairingCode() == protocol.PairingCode("ab", a.identity.PublicKey) {
		t.Fatal("the code must follow the certificate actually pinned")
	}
}

func TestSelfUpdateDisabledLeavesOutTheFeature(t *testing.T) {
	has := func(a *Agent) bool {
		for _, f := range a.agentFeatures() {
			if f == domain.FeatureSelfUpdatePath {
				return true
			}
		}
		return false
	}
	if !has(newPairingAgent(t, Config{})) {
		t.Fatal("agents offer self-update by default")
	}
	if has(newPairingAgent(t, Config{SelfUpdateDisabled: true})) {
		t.Fatal("an agent that can't replace itself must not offer self-update")
	}
}

// Seen on a real phone: a worker that learned the manager's certificate at
// its first connection could not fetch its tasks' input files, because the
// file client still checked against the (empty) start-up fingerprint.
func TestFileClientPinsTheLearnedCertificate(t *testing.T) {
	cert := []byte("the manager's certificate")
	sum := sha256.Sum256(cert)
	learned := ""
	a := newPairingAgent(t, Config{Pairing: true, ManagerFingerprintFunc: func() string { return learned }})
	learned = hex.EncodeToString(sum[:]) // the first connection happened
	tr := a.selfUpdateHTTPClient().Transport.(*http.Transport)
	if err := tr.TLSClientConfig.VerifyPeerCertificate([][]byte{cert}, nil); err != nil {
		t.Fatalf("the learned certificate was refused: %v", err)
	}
	if err := tr.TLSClientConfig.VerifyPeerCertificate([][]byte{[]byte("another one")}, nil); err == nil {
		t.Fatal("a different certificate was accepted")
	}
}
