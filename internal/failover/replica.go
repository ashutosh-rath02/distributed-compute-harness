package failover

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
	"home-harness/internal/mtls"
	"home-harness/internal/store/persistent"
)

// DefaultInterval is how often a standby checks the primary for changes.
const DefaultInterval = 15 * time.Second

// Options configure a standby: where its copy of the state goes (the
// same paths it will serve from once promoted) and how it behaves.
type Options struct {
	DBPath    string
	TLSDir    string
	AIKeyFile string
	// Artifacts receives a copy of the primary's stored workload files;
	// nil leaves them out (jobs needing them then fail after a failover).
	Artifacts *artifacts.Store
	// Interval between checks for changes (DefaultInterval when 0).
	Interval time.Duration
	// AutoPromoteAfter promotes this standby by itself once the primary
	// has not answered for this long; 0 (the default) never does. It
	// can't tell a dead primary from a broken network between the two,
	// which is why it is off unless asked for.
	AutoPromoteAfter time.Duration
	// Name and Addr describe this standby to the primary: Addr is its
	// agent-facing address, which agents try once the primary is gone.
	Name string
	Addr string
}

// Replica is a running standby: it keeps a copy of the primary's state
// and doesn't serve agents until it is promoted.
type Replica struct {
	opt     Options
	client  *http.Client
	promote chan promoteRequest
	started time.Time

	mu          sync.Mutex
	role        Role
	etag        string
	lastContact time.Time // the primary last answered at all
	lastCheck   time.Time // the copy was last confirmed current
	reachable   bool
	lastErr     string
}

type promoteRequest struct {
	force  bool
	actor  string
	result chan error
}

// NewReplica prepares a standby for role (which must be a standby role).
func NewReplica(opt Options, role Role) *Replica {
	if opt.Interval <= 0 {
		opt.Interval = DefaultInterval
	}
	return &Replica{opt: opt, role: role, client: NewClient(role.Fingerprint), promote: make(chan promoteRequest), started: time.Now()}
}

// errUnpaired: the primary refused this standby's secret.
var errUnpaired = errors.New("the primary no longer accepts this standby (another standby was added in its place, or it was removed)")

// Run keeps the copy current until the standby is promoted (true) or ctx
// ends. It never listens for agents.
func (r *Replica) Run(ctx context.Context) (promoted bool, err error) {
	log.Printf("manager: standby of %s: copying its state every %s (not serving agents)", r.role.Primary, r.opt.Interval)
	r.round(ctx)
	tick := time.NewTicker(r.opt.Interval)
	defer tick.Stop()
	for {
		if r.dueForAutoPromotion(time.Now()) {
			if err := r.doPromote(false, "auto-promote", true); err != nil {
				log.Printf("manager: standby: automatic promotion failed: %v", err)
			} else {
				return true, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case req := <-r.promote:
			err := r.handlePromote(ctx, req)
			req.result <- err
			if err == nil {
				return true, nil
			}
		case <-tick.C:
			r.round(ctx)
		}
	}
}

// round checks the primary once.
func (r *Replica) round(ctx context.Context) {
	reached, err := r.syncOnce(ctx)
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reachable = reached
	if reached {
		r.lastContact = now
	}
	if err == nil {
		r.lastCheck, r.lastErr = now, ""
		return
	}
	msg := err.Error()
	if msg != r.lastErr {
		log.Printf("manager: standby: %v", err)
	}
	r.lastErr = msg
}

// syncOnce asks the primary for its state, applying it if it changed.
// reached reports whether the primary answered at all (any HTTP answer
// over the pinned link: a refusal still means it is alive).
func (r *Replica) syncOnce(ctx context.Context) (reached bool, err error) {
	r.mu.Lock()
	role, etag := r.role, r.etag
	r.mu.Unlock()
	if role.Primary == "" {
		return false, errors.New("no primary is known: add this manager again as a standby (harnessctl standby add on the active one)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, linkURL(role.Primary, RouteSync), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+role.Secret)
	if etag != "" && !role.CopiedAt.IsZero() {
		req.Header.Set("If-None-Match", etag)
	}
	if role.SteppedDown {
		req.Header.Set(HeaderSteppedDown, strconv.FormatUint(role.SteppedDownFrom, 10))
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("the primary at %s doesn't answer: %w", role.Primary, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		if role.SteppedDown {
			r.updateRole(func(ro *Role) { ro.SteppedDown, ro.SteppedDownFrom = false, 0 })
		}
		return true, nil
	case http.StatusOK:
		if err := r.apply(ctx, resp.Body, resp.Header.Get("ETag")); err != nil {
			return true, fmt.Errorf("copy from %s: %w", role.Primary, err)
		}
		return true, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		if !role.Unpaired {
			r.updateRole(func(ro *Role) { ro.Unpaired = true })
		}
		return true, errUnpaired
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return true, fmt.Errorf("the primary answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
}

// apply installs one sync bundle: the database (replaced whole, by rename,
// only once it has been checked to open), the TLS identity (only the one
// this standby pinned), the AI key, and the role file's copy of the term
// and pairing token; then mirrors the stored files.
func (r *Replica) apply(ctx context.Context, body io.Reader, etag string) error {
	zipPath := r.opt.DBPath + ".incoming.zip"
	if err := writeFileSynced(zipPath, io.LimitReader(body, maxDBBytes+maxStateBytes+4*maxPEMBytes)); err != nil {
		return err
	}
	defer os.Remove(zipPath)
	b, err := OpenBundle(zipPath)
	if err != nil {
		return err
	}
	defer b.Close()

	r.mu.Lock()
	role := r.role
	r.mu.Unlock()
	cert, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM)
	if err != nil {
		return fmt.Errorf("the identity in the bundle is unusable: %w", err)
	}
	if got := mtls.Fingerprint(cert); got != role.Fingerprint {
		return fmt.Errorf("the bundle carries identity %s, not the pinned %s: refusing it", got, role.Fingerprint)
	}

	dbTmp := r.opt.DBPath + ".incoming"
	if err := os.MkdirAll(filepath.Dir(r.opt.DBPath), 0o700); err != nil {
		return err
	}
	if err := b.ExtractDB(dbTmp); err != nil {
		return fmt.Errorf("unpack database: %w", err)
	}
	check, err := bbolt.Open(dbTmp, 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		os.Remove(dbTmp)
		return fmt.Errorf("the copied database doesn't open: %w", err)
	}
	check.Close()
	if err := os.Rename(dbTmp, r.opt.DBPath); err != nil {
		os.Remove(dbTmp)
		return fmt.Errorf("replace database: %w", err)
	}
	if current, err := os.ReadFile(filepath.Join(r.opt.TLSDir, "manager-cert.pem")); err != nil || !bytes.Equal(current, b.CertPEM) {
		if _, err := mtls.Install(r.opt.TLSDir, b.CertPEM, b.KeyPEM); err != nil {
			return err
		}
	}
	if b.State.AIKey != "" && r.opt.AIKeyFile != "" {
		if current, err := os.ReadFile(r.opt.AIKeyFile); err != nil || string(bytes.TrimSpace(current)) != b.State.AIKey {
			if err := writeFileSynced(r.opt.AIKeyFile, bytes.NewReader([]byte(b.State.AIKey+"\n"))); err != nil {
				return fmt.Errorf("write AI key: %w", err)
			}
		}
	}
	first := role.CopiedAt.IsZero()
	r.updateRole(func(ro *Role) {
		ro.PairingToken = b.State.PairingToken
		ro.SeenTerm = max(ro.SeenTerm, b.State.Term)
		ro.CopiedAt = time.Now().UTC()
		ro.SteppedDown, ro.SteppedDownFrom = false, 0
		ro.Unpaired = false
	})
	r.mu.Lock()
	r.etag = etag
	r.mu.Unlock()
	if first {
		log.Printf("manager: standby: copied the primary's state (term %d)", b.State.Term)
	}
	r.mirrorArtifacts(ctx, b.State.Artifacts)
	return nil
}

// mirrorArtifacts makes the local file store hold exactly the primary's
// files: drops the ones it no longer has, fetches (hash-checked) the ones
// missing here.
func (r *Replica) mirrorArtifacts(ctx context.Context, want []string) {
	store := r.opt.Artifacts
	if store == nil {
		return
	}
	wanted := make(map[string]bool, len(want))
	for _, sha := range want {
		if domain.ValidSHA256(sha) {
			wanted[sha] = true
		}
	}
	local, err := store.List()
	if err != nil {
		log.Printf("manager: standby: list stored files: %v", err)
		return
	}
	have := make(map[string]bool, len(local))
	for _, in := range local {
		have[in.SHA256] = true
		if !wanted[in.SHA256] {
			store.Delete(in.SHA256)
		}
	}
	r.mu.Lock()
	role := r.role
	r.mu.Unlock()
	for sha := range wanted {
		if have[sha] || ctx.Err() != nil {
			continue
		}
		if err := r.fetchArtifact(ctx, role, sha); err != nil {
			log.Printf("manager: standby: copy stored file %s: %v", sha[:12], err)
		}
	}
}

func (r *Replica) fetchArtifact(ctx context.Context, role Role, sha string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, linkURL(role.Primary, RouteArtifacts+sha), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+role.Secret)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // deleted on the primary since: the next round drops it
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("primary answered %s", resp.Status)
	}
	_, err = r.opt.Artifacts.Put(resp.Body, sha)
	return err
}

func (r *Replica) updateRole(change func(*Role)) {
	r.mu.Lock()
	change(&r.role)
	role := r.role
	r.mu.Unlock()
	if err := SaveRole(r.opt.DBPath, &role); err != nil {
		log.Printf("manager: standby: %v", err)
	}
}

func (r *Replica) dueForAutoPromotion(now time.Time) bool {
	if r.opt.AutoPromoteAfter <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role.CopiedAt.IsZero() || r.role.Unpaired {
		return false // nothing to serve, or another standby owns the pair now
	}
	last := r.lastContact
	if last.IsZero() {
		last = r.started
	}
	return now.Sub(last) >= r.opt.AutoPromoteAfter
}

// Promote asks the running standby to take over. Refused while the
// primary still answers, unless force (a planned switch-over: the copy is
// brought current first, and the primary steps down when it next checks,
// within ~10 s). Without a copy it is refused too, unless force and this
// manager has a state of its own (a stepped-down primary whose successor
// is gone). actor goes into the audit entry.
func (r *Replica) Promote(ctx context.Context, force bool, actor string) error {
	req := promoteRequest{force: force, actor: actor, result: make(chan error, 1)}
	select {
	case r.promote <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrPromotionRefused wraps every reason a promotion was refused.
var ErrPromotionRefused = errors.New("promotion refused")

func (r *Replica) handlePromote(ctx context.Context, req promoteRequest) error {
	r.mu.Lock()
	role := r.role
	r.mu.Unlock()
	if role.Unpaired && !req.force {
		return fmt.Errorf("%w: %v; promoting it could run two managers at the same term", ErrPromotionRefused, errUnpaired)
	}
	// One more look at the primary: is it really gone? (With force, this
	// also brings the copy up to date.)
	reached, err := r.syncOnce(ctx)
	r.mu.Lock()
	r.reachable = reached
	if reached {
		r.lastContact = time.Now()
	}
	if err == nil {
		r.lastCheck = time.Now()
	}
	role = r.role
	r.mu.Unlock()
	if reached && !req.force {
		return fmt.Errorf("%w: the primary at %s still answers, and two active managers would split the fleet. Stop it first, or force a planned switch-over (it then steps down within about 10 seconds)", ErrPromotionRefused, role.Primary)
	}
	if role.CopiedAt.IsZero() {
		if !req.force {
			return fmt.Errorf("%w: this standby has no copy of the primary's state yet", ErrPromotionRefused)
		}
		// The way back when the manager that superseded this one is gone
		// for good: a stepped-down primary serves its own last state, at a
		// term above the one it stepped down for (so devices accept it). A
		// standby with no state at all has nothing to serve.
		for _, p := range []string{r.opt.DBPath, filepath.Join(r.opt.TLSDir, "manager-key.pem")} {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("%w: this standby has no state of its own to serve (%s is missing)", ErrPromotionRefused, filepath.Base(p))
			}
		}
		log.Printf("manager: standby: forced promotion without a current copy: serving this manager's own last state")
	}
	return r.doPromote(req.force, req.actor, false)
}

// doPromote makes the copy the active state: a term above every one this
// standby has seen, written (fsynced) to the copied database before
// anything serves from it, the old primary recorded as the peer, an audit
// entry, and the role file. Only the database write has to land first: a
// crash before the role file is rewritten restarts this manager as a
// standby, which never served at the new term.
func (r *Replica) doPromote(force bool, actor string, automatic bool) error {
	r.mu.Lock()
	role := r.role
	r.mu.Unlock()
	store, err := persistent.Open(r.opt.DBPath)
	if err != nil {
		return err
	}
	copied, err := store.GetTerm()
	if err != nil {
		store.Close()
		return err
	}
	term := max(role.SeenTerm, copied) + 1
	if err := store.PutTerm(term); err != nil {
		store.Close()
		return fmt.Errorf("record the new term: %w", err)
	}
	pair, _, err := store.GetStandbyPair()
	if err == nil {
		pair.Secret, pair.PeerAddr, pair.PeerName = role.Secret, role.Primary, "previous primary"
		err = store.PutStandbyPair(pair)
	}
	if err != nil {
		store.Close()
		return fmt.Errorf("record the old primary as the standby: %w", err)
	}
	if _, err := store.AppendAudit(domain.AuditEntry{
		Log: domain.AuditSecurity, Time: time.Now().UTC(), Kind: "standby.promoted", Actor: actor,
		Detail: map[string]any{"term": term, "previousTerm": copied, "previousPrimary": role.Primary, "forced": force, "automatic": automatic},
	}, 5000); err != nil {
		log.Printf("manager: standby: audit promotion: %v", err)
	}
	if err := store.Close(); err != nil {
		return err
	}
	r.updateRole(func(ro *Role) {
		ro.Role = RoleActive
		ro.SeenTerm = term
	})
	// The Android app and the Windows launcher restart the manager with
	// the pairing-token file beside the database: make it the fleet's.
	if role.PairingToken != "" {
		tokenFile := filepath.Join(filepath.Dir(r.opt.DBPath), "pairing-token")
		if _, err := os.Stat(tokenFile); err == nil {
			if err := writeFileSynced(tokenFile, bytes.NewReader([]byte(role.PairingToken+"\n"))); err != nil {
				log.Printf("manager: standby: update %s: %v", tokenFile, err)
			}
		}
	}
	how := "promoted by the operator"
	if automatic {
		how = fmt.Sprintf("promoted itself: the primary didn't answer for %s", r.opt.AutoPromoteAfter)
	}
	log.Printf("manager: standby %s: now the active manager at term %d (previous primary %s)", how, term, role.Primary)
	return nil
}

// ReplicaStatus is a standby's GET /standby.
type ReplicaStatus struct {
	Role     string `json:"role"`
	Primary  string `json:"primary"`
	Term     uint64 `json:"term"`
	Copied   bool   `json:"copied"`
	Unpaired bool   `json:"unpaired,omitempty"`
	// LastCopyAt: the state last changed and was copied. LastContactAt:
	// the primary last answered. LagSeconds: since the copy was last
	// confirmed current (-1: never).
	LastCopyAt              time.Time `json:"lastCopyAt,omitempty"`
	LastContactAt           time.Time `json:"lastContactAt,omitempty"`
	LagSeconds              int64     `json:"lagSeconds"`
	PrimaryReachable        bool      `json:"primaryReachable"`
	AutoPromoteAfterSeconds int64     `json:"autoPromoteAfterSeconds,omitempty"`
	Error                   string    `json:"error,omitempty"`
}

// Status reports the standby's state (never the secret).
func (r *Replica) Status() ReplicaStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	lag := int64(-1)
	if !r.lastCheck.IsZero() {
		lag = int64(time.Since(r.lastCheck).Seconds())
	}
	return ReplicaStatus{
		Role: RoleStandby, Primary: r.role.Primary, Term: r.role.SeenTerm, Copied: !r.role.CopiedAt.IsZero(), Unpaired: r.role.Unpaired,
		LastCopyAt: r.role.CopiedAt, LastContactAt: r.lastContact, LagSeconds: lag, PrimaryReachable: r.reachable,
		AutoPromoteAfterSeconds: int64(r.opt.AutoPromoteAfter.Seconds()), Error: r.lastErr,
	}
}

func writeFileSynced(path string, r io.Reader) error {
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
