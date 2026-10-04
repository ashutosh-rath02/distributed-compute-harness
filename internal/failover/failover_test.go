package failover

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/mtls"
	"home-harness/internal/store/persistent"
)

func testCert(t *testing.T) (tls.Certificate, []byte, []byte, string) {
	t.Helper()
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := mtls.PEM(cert)
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM, keyPEM, mtls.Fingerprint(cert)
}

// A term proof holds only for its term and its key; nothing else passes.
func TestTermProof(t *testing.T) {
	a, _, _, _ := testCert(t)
	b, _, _, _ := testCert(t)
	proof, err := SignTerm(a, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyTerm(a, 3, proof) {
		t.Fatal("a valid proof didn't verify")
	}
	for name, ok := range map[string]bool{
		"other term":  VerifyTerm(a, 4, proof),
		"other key":   VerifyTerm(b, 3, proof),
		"garbage":     VerifyTerm(a, 3, []byte("garbage")),
		"empty":       VerifyTerm(a, 3, nil),
		"no identity": VerifyTerm(tls.Certificate{}, 3, proof),
	} {
		if ok {
			t.Errorf("%s: verified", name)
		}
	}
}

// Only an active peer at a higher term, with a valid proof, supersedes.
func TestSupersedes(t *testing.T) {
	cert, _, _, _ := testCert(t)
	other, _, _, _ := testCert(t)
	p2, _ := SignTerm(cert, 2)
	foreign, _ := SignTerm(other, 2)
	cases := []struct {
		name string
		own  uint64
		st   Status
		want bool
	}{
		{"higher, valid", 1, Status{Role: RoleActive, Term: 2, TermProof: p2}, true},
		{"equal", 2, Status{Role: RoleActive, Term: 2, TermProof: p2}, false},
		{"lower", 3, Status{Role: RoleActive, Term: 2, TermProof: p2}, false},
		{"not active", 1, Status{Role: RoleStandby, Term: 2, TermProof: p2}, false},
		{"no proof", 1, Status{Role: RoleActive, Term: 2}, false},
		{"foreign key", 1, Status{Role: RoleActive, Term: 2, TermProof: foreign}, false},
	}
	for _, c := range cases {
		if got := Supersedes(cert, c.own, c.st); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func writeTestBundle(t *testing.T, path string, st State, db, certPEM, keyPEM []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteBundle(&buf, st, bytes.NewReader(db), certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBundleRoundTrip(t *testing.T) {
	_, certPEM, keyPEM, _ := testCert(t)
	path := filepath.Join(t.TempDir(), "b.zip")
	st := State{Term: 4, PairingToken: "pt", AIKey: "ak", Artifacts: []string{strings.Repeat("a", 64)}}
	writeTestBundle(t, path, st, []byte("database bytes"), certPEM, keyPEM)
	b, err := OpenBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.State.Term != 4 || b.State.PairingToken != "pt" || b.State.AIKey != "ak" || len(b.State.Artifacts) != 1 {
		t.Fatalf("state: %+v", b.State)
	}
	if !bytes.Equal(b.CertPEM, certPEM) || !bytes.Equal(b.KeyPEM, keyPEM) {
		t.Fatal("PEMs changed")
	}
	dest := filepath.Join(t.TempDir(), "db")
	if err := b.ExtractDB(dest); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "database bytes" {
		t.Fatalf("db: %q", data)
	}
}

// Names off the wire are never used: unknown entries are ignored, a
// missing or doubled entry refuses the whole bundle.
func TestBundleRefusesOddShapes(t *testing.T) {
	_, certPEM, keyPEM, _ := testCert(t)
	build := func(entries ...string) string {
		path := filepath.Join(t.TempDir(), "b.zip")
		f, _ := os.Create(path)
		zw := zip.NewWriter(f)
		for _, name := range entries {
			w, _ := zw.Create(name)
			switch name {
			case entryState:
				w.Write([]byte(`{"term":1}`))
			case entryCert:
				w.Write(certPEM)
			case entryKey:
				w.Write(keyPEM)
			default:
				w.Write([]byte("x"))
			}
		}
		zw.Close()
		f.Close()
		return path
	}
	b, err := OpenBundle(build(entryState, entryCert, entryKey, entryDB, "../evil", "tls/../../x"))
	if err != nil {
		t.Fatalf("unknown entries should be ignored: %v", err)
	}
	b.Close()
	if _, err := OpenBundle(build(entryState, entryCert, entryKey)); err == nil {
		t.Fatal("a bundle without a database was accepted")
	}
	if _, err := OpenBundle(build(entryState, entryCert, entryKey, entryDB, entryDB)); err == nil {
		t.Fatal("a bundle with two databases was accepted")
	}
}

func TestRoleFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state", "manager.db")
	if r, err := LoadRole(db); r != nil || err != nil {
		t.Fatalf("no file: %v %v", r, err)
	}
	want := &Role{Role: RoleStandby, Primary: "10.0.0.1:7420", Fingerprint: "ab", Secret: "s", SeenTerm: 2}
	if err := SaveRole(db, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRole(db)
	if err != nil || got.Primary != want.Primary || got.SeenTerm != 2 || got.Role != RoleStandby {
		t.Fatalf("got %+v, %v", got, err)
	}
	os.WriteFile(RolePath(db), []byte(`{"role":"boss"}`), 0o600)
	if _, err := LoadRole(db); err == nil {
		t.Fatal("an unknown role was accepted")
	}
}

// newTestReplica is a standby whose copy is in place (db + identity), with
// a primary at an address that refuses connections.
func newTestReplica(t *testing.T, role Role, dbTerm uint64) (*Replica, Options) {
	t.Helper()
	dir := t.TempDir()
	opt := Options{DBPath: filepath.Join(dir, "manager.db"), TLSDir: filepath.Join(dir, "tls"), Interval: time.Hour}
	store, err := persistent.Open(opt.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	store.PutTerm(dbTerm)
	store.PutStandbyPair(domain.StandbyPair{Secret: role.Secret, PeerAddr: "10.9.9.9:7420", PeerName: "this standby"})
	store.Close()
	if role.Primary == "" {
		role.Primary = "127.0.0.1:1"
	}
	role.Role = RoleStandby
	if err := SaveRole(opt.DBPath, &role); err != nil {
		t.Fatal(err)
	}
	return NewReplica(opt, role), opt
}

func runReplica(t *testing.T, r *Replica) (context.CancelFunc, chan bool) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		promoted, _ := r.Run(ctx)
		done <- promoted
	}()
	t.Cleanup(cancel)
	return cancel, done
}

// A promotion goes above every term the standby has seen — the copy's and
// any it learned otherwise — and lands in the database (with the old
// primary as the peer and an audit entry) before the role file says
// active.
func TestPromotionTerm(t *testing.T) {
	for _, c := range []struct{ seen, db, want uint64 }{{5, 3, 6}, {2, 3, 4}, {0, 0, 1}} {
		r, opt := newTestReplica(t, Role{Secret: "sec", SeenTerm: c.seen, CopiedAt: time.Now()}, c.db)
		_, done := runReplica(t, r)
		if err := r.Promote(context.Background(), false, "operator-token"); err != nil {
			t.Fatalf("promote: %v", err)
		}
		if !<-done {
			t.Fatal("Run didn't report the promotion")
		}
		store, err := persistent.Open(opt.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		term, _ := store.GetTerm()
		pair, _, _ := store.GetStandbyPair()
		audit, _ := store.ListAudit(domain.AuditSecurity, 10)
		store.Close()
		if term != c.want {
			t.Errorf("seen %d, db %d: term %d, want %d", c.seen, c.db, term, c.want)
		}
		if pair.PeerAddr != "127.0.0.1:1" || pair.Secret != "sec" {
			t.Errorf("peer after promotion: %+v", pair)
		}
		if len(audit) != 1 || audit[0].Kind != "standby.promoted" || audit[0].Actor != "operator-token" {
			t.Errorf("audit: %+v", audit)
		}
		role, _ := LoadRole(opt.DBPath)
		if role.Role != RoleActive || role.SeenTerm != c.want {
			t.Errorf("role after promotion: %+v", role)
		}
	}
}

func TestPromotionRefused(t *testing.T) {
	// No copy yet.
	r, opt := newTestReplica(t, Role{Secret: "s"}, 0)
	runReplica(t, r)
	if err := r.Promote(context.Background(), true, "x"); !errors.Is(err, ErrPromotionRefused) {
		t.Fatalf("promotion without a copy: %v", err)
	}
	if role, _ := LoadRole(opt.DBPath); role.Role != RoleStandby {
		t.Fatal("a refused promotion changed the role")
	}
	// Replaced by another standby: refused unless forced.
	r, _ = newTestReplica(t, Role{Secret: "s", CopiedAt: time.Now(), Unpaired: true}, 0)
	_, done := runReplica(t, r)
	if err := r.Promote(context.Background(), false, "x"); !errors.Is(err, ErrPromotionRefused) {
		t.Fatalf("promotion of an unpaired standby: %v", err)
	}
	if err := r.Promote(context.Background(), true, "x"); err != nil || !<-done {
		t.Fatalf("forced promotion: %v", err)
	}
	// A stepped-down primary whose successor is gone for good: no copy,
	// but a state and identity of its own. Forced, it serves that, above
	// the term it stepped down for.
	r, opt = newTestReplica(t, Role{Secret: "s", SeenTerm: 4, SteppedDown: true}, 1)
	_, certPEM, keyPEM, _ := testCert(t)
	if _, err := mtls.Install(opt.TLSDir, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	_, done = runReplica(t, r)
	if err := r.Promote(context.Background(), false, "x"); !errors.Is(err, ErrPromotionRefused) {
		t.Fatalf("unforced promotion without a copy: %v", err)
	}
	if err := r.Promote(context.Background(), true, "x"); err != nil || !<-done {
		t.Fatalf("forced promotion of a stepped-down primary: %v", err)
	}
	if role, _ := LoadRole(opt.DBPath); role.Role != RoleActive || role.SeenTerm != 5 {
		t.Fatalf("role: %+v", role)
	}
}

// Automatic promotion never happens without a copy, for a standby the
// primary no longer accepts, or while the primary answers.
func TestAutoPromotionConditions(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name        string
		role        Role
		lastContact time.Time
		want        bool
	}{
		{"silent long enough", Role{CopiedAt: now}, now.Add(-2 * time.Minute), true},
		{"answering", Role{CopiedAt: now}, now.Add(-10 * time.Second), false},
		{"no copy", Role{}, now.Add(-2 * time.Minute), false},
		{"unpaired", Role{CopiedAt: now, Unpaired: true}, now.Add(-2 * time.Minute), false},
	} {
		r := NewReplica(Options{AutoPromoteAfter: time.Minute}, c.role)
		r.lastContact = c.lastContact
		if got := r.dueForAutoPromotion(now); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	r := NewReplica(Options{}, Role{CopiedAt: now})
	r.lastContact = now.Add(-time.Hour)
	if r.dueForAutoPromotion(now) {
		t.Error("promoted itself with automatic promotion off")
	}
}

// A bundle carrying any identity but the pinned one is refused whole:
// nothing of it is installed.
func TestApplyRefusesAnotherIdentity(t *testing.T) {
	_, certPEM, keyPEM, _ := testCert(t)
	_, _, _, pinned := testCert(t)
	r, opt := newTestReplica(t, Role{Secret: "s", Fingerprint: pinned}, 0)
	before, _ := os.ReadFile(opt.DBPath)
	var buf bytes.Buffer
	WriteBundle(&buf, State{Term: 9, PairingToken: "evil"}, bytes.NewReader([]byte("not a db")), certPEM, keyPEM)
	if err := r.apply(context.Background(), &buf, `"1-x"`); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("apply: %v", err)
	}
	after, _ := os.ReadFile(opt.DBPath)
	if !bytes.Equal(before, after) {
		t.Fatal("the database was replaced")
	}
	if _, err := os.Stat(filepath.Join(opt.TLSDir, "manager-key.pem")); err == nil {
		t.Fatal("a foreign key was installed")
	}
	if role, _ := LoadRole(opt.DBPath); role.PairingToken == "evil" || role.SeenTerm == 9 {
		t.Fatalf("role took the foreign state: %+v", role)
	}
}

// Becoming a standby replaces the database and identity: refused over an
// existing one unless forced.
func TestEnrollKeepsExistingStateUnlessForced(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "manager.db")
	os.WriteFile(db, []byte("existing"), 0o600)
	_, err := enroll(context.Background(), PhaseOptions{Options: Options{DBPath: db, TLSDir: filepath.Join(dir, "tls")}, Primary: "127.0.0.1:1", Token: "t", Fingerprint: "ab"})
	if err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("enroll over an existing database: %v", err)
	}
	_, err = enroll(context.Background(), PhaseOptions{Options: Options{DBPath: db, TLSDir: filepath.Join(dir, "tls")}, Primary: "127.0.0.1:1", Token: "t", Fingerprint: "ab", Force: true})
	if err == nil || strings.Contains(err.Error(), "-force") {
		t.Fatalf("forced enroll should get as far as the (unreachable) primary: %v", err)
	}
}

// The role a stepped-down manager writes: a standby of the newer manager,
// with nothing to promote until it has copied that manager's state.
func TestStepDownRole(t *testing.T) {
	db := filepath.Join(t.TempDir(), "manager.db")
	SaveRole(db, &Role{Role: RoleActive, PairingToken: "fleet", SeenTerm: 1, CopiedAt: time.Now()})
	role, err := StepDown(db, "fp", domain.StandbyPair{Secret: "s", PeerAddr: "10.0.0.2:7420"}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := LoadRole(db)
	if got.Role != RoleStandby || got.Primary != "10.0.0.2:7420" || got.SeenTerm != 2 || !got.SteppedDown || got.SteppedDownFrom != 1 ||
		got.PairingToken != "fleet" || !got.CopiedAt.IsZero() || got.Secret != "s" || role.Fingerprint != "fp" {
		t.Fatalf("role: %+v", got)
	}
}
