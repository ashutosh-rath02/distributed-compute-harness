package manager

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/failover"
	"home-harness/internal/mtls"
	"home-harness/internal/protocol"
)

// The active side of a standby manager (roadmap item 17). The fencing
// rule this implements is spelled out in internal/failover's package
// comment; in short: this manager serves only while no term above its
// own is known, and steps down — refusing every device, then running as
// its peer's standby — the moment one is (a device's REGISTER carrying a
// signed higher term, or its peer reporting it took over).
//
// The standby link is a set of routes on the agent-facing listener
// (StandbyHandler), only over TLS: it hands out the manager's private key,
// the pairing token, the AI key and the whole database. Every route needs
// a bearer credential: the one-time enrollment token (POST /standby on the
// operator API makes one) to enroll, the pair secret after that. Nothing
// here is ever on the operator API.

const (
	defaultPeerCheckInterval = 10 * time.Second
	standbyEnrollmentTTL     = 10 * time.Minute
	actorStandby             = "standby"
	actorFailover            = "failover"
)

// failoverStore is what the standby link needs from the store beyond
// PersistentStore; internal/store/persistent has it. A manager whose store
// lacks it (tests, embedding) can't have a standby.
type failoverStore interface {
	GetTerm() (uint64, error)
	GetStandbyPair() (domain.StandbyPair, bool, error)
	PutStandbyPair(domain.StandbyPair) error
	DeleteStandbyPair() error
	Snapshot(w io.Writer) (uint64, error)
	Version() (uint64, error)
	Path() string
}

type failoverState struct {
	mu    sync.Mutex
	term  uint64
	proof []byte
	pair  *domain.StandbyPair
	// The pending one-time enrollment (only its hash is kept).
	enrollHash  [32]byte
	enrollUntil time.Time
	// What the standby last fetched or confirmed, and when.
	lastSync time.Time
	lastETag string
	// fenced: a newer active manager exists; this one serves nobody.
	fenced bool
	client *http.Client
}

func (s *Server) failoverStore() (failoverStore, bool) {
	fs, ok := s.store.(failoverStore)
	return fs, ok && len(s.cfg.TLSCert.Certificate) > 0
}

// loadFailover reads the term and the pair at start (Run).
func (s *Server) loadFailover() error {
	fs, ok := s.store.(failoverStore)
	if !ok {
		return nil
	}
	term, err := fs.GetTerm()
	if err != nil {
		return fmt.Errorf("manager: load failover term: %w", err)
	}
	pair, found, err := fs.GetStandbyPair()
	if err != nil {
		return fmt.Errorf("manager: load standby: %w", err)
	}
	f := s.failover
	f.mu.Lock()
	defer f.mu.Unlock()
	f.term = term
	if found {
		f.pair = &pair
	}
	if term > 0 && len(s.cfg.TLSCert.Certificate) > 0 {
		if f.proof, err = failover.SignTerm(s.cfg.TLSCert, term); err != nil {
			return fmt.Errorf("manager: sign failover term: %w", err)
		}
	}
	if term > 0 {
		log.Printf("manager: failover term %d", term)
	}
	return nil
}

// Term is this manager's failover term (0: never failed over).
func (s *Server) Term() uint64 {
	s.failover.mu.Lock()
	defer s.failover.mu.Unlock()
	return s.failover.term
}

// SteppedDown reports whether this manager learned of a newer active one
// and stopped serving.
func (s *Server) SteppedDown() bool {
	s.failover.mu.Lock()
	defer s.failover.mu.Unlock()
	return s.failover.fenced
}

// failoverView is what failover.v1 agents are told: the term, its proof,
// and where the pair's managers listen (this one first).
func (s *Server) failoverView() (uint64, []byte, []string) {
	f := s.failover
	f.mu.Lock()
	defer f.mu.Unlock()
	var addrs []string
	if s.cfg.AdvertiseAddr != "" {
		addrs = append(addrs, s.cfg.AdvertiseAddr)
	}
	if f.pair != nil && f.pair.PeerAddr != "" && f.pair.PeerAddr != s.cfg.AdvertiseAddr {
		addrs = append(addrs, f.pair.PeerAddr)
	}
	return f.term, f.proof, addrs
}

// registerAck is the REGISTER_ACK for an admitted node: failover.v1
// agents also get the term and the pair's addresses.
func (s *Server) registerAck(id domain.NodeID, m domain.Manifest) protocol.RegisterAckPayload {
	ack := protocol.RegisterAckPayload{NodeID: id, ServerTime: time.Now().UTC()}
	if m.HasAgentFeature(domain.FeatureFailover) {
		ack.Term, ack.TermProof, ack.Managers = s.failoverView()
	}
	return ack
}

// refuseIfSuperseded is the REGISTER-time fence: a device that has seen a
// term above this manager's — proven by the pair key's signature, so no
// device can make one up — means another manager took over, and this one
// steps down. A manager that stepped down refuses every device.
func (s *Server) refuseIfSuperseded(ctx context.Context, conn domain.Conn, dest domain.NodeID, term uint64, proof []byte) bool {
	f := s.failover
	f.mu.Lock()
	fenced, own := f.fenced, f.term
	f.mu.Unlock()
	if !fenced && term > own && failover.VerifyTerm(s.cfg.TLSCert, term, proof) {
		s.stepDown(term, "a device has been with a newer active manager")
		fenced = true
	}
	if !fenced {
		return false
	}
	s.send(ctx, conn, protocol.MsgRegisterReject, domain.ManagerNodeID, dest,
		protocol.RegisterRejectPayload{Reason: "this manager is not the active one: its standby took over", NotActive: true})
	return true
}

// stepDown stops this manager serving, once: every device is dropped and
// refused from now on, and Config.StepDown (cmd/manager) is told, which
// shuts the manager down and runs it as the newer manager's standby. Only
// signals: never blocks its caller (a REGISTER or the peer check).
func (s *Server) stepDown(newTerm uint64, why string) {
	f := s.failover
	f.mu.Lock()
	if f.fenced {
		f.mu.Unlock()
		return
	}
	f.fenced = true
	own := f.term
	var pair domain.StandbyPair
	if f.pair != nil {
		pair = *f.pair
	}
	f.mu.Unlock()
	log.Printf("manager: STEPPING DOWN: %s (term %d; this manager's is %d). It no longer serves devices", why, newTerm, own)
	// This manager's own log is replaced once it copies the active one's;
	// the active one records the step-down too (HeaderSteppedDown).
	s.audit(domain.AuditAdmissions, "standby.stepped-down", "", actorFailover, map[string]any{"term": own, "newTerm": newTerm, "peer": pair.PeerAddr, "reason": why})
	s.publish(domain.EventStandby, "", map[string]any{"event": "stepped-down", "term": newTerm})
	if s.cfg.StepDown != nil {
		go s.cfg.StepDown(pair, own, newTerm)
	}
	for _, rec := range s.Registry.List() {
		if conn := s.Registry.ConnOf(rec.Node.Identity.NodeID); conn != nil {
			go conn.Close()
		}
	}
}

func (s *Server) peerClient() *http.Client {
	f := s.failover
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client == nil {
		f.client = failover.NewClient(s.cfg.Fingerprint)
	}
	return f.client
}

// watchPeer asks the standby, every PeerCheckInterval, whether it took
// over (it only answers once promoted: a standby doesn't listen). It is
// the fence that works when no device moves: a forced switch-over, or the
// standby promoted while this manager's PC slept.
func (s *Server) watchPeer(ctx context.Context) {
	interval := s.cfg.PeerCheckInterval
	if interval <= 0 {
		interval = defaultPeerCheckInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		f := s.failover
		f.mu.Lock()
		fenced, own := f.fenced, f.term
		var pair domain.StandbyPair
		if f.pair != nil {
			pair = *f.pair
		}
		f.mu.Unlock()
		if fenced || pair.PeerAddr == "" || len(s.cfg.TLSCert.Certificate) == 0 {
			continue
		}
		st, err := failover.FetchStatus(ctx, s.peerClient(), pair.PeerAddr, pair.Secret)
		if err != nil {
			continue
		}
		if failover.Supersedes(s.cfg.TLSCert, own, st) {
			s.stepDown(st.Term, "the standby at "+pair.PeerAddr+" took over")
		}
	}
}

// broadcastManagers tells connected failover.v1 agents the pair's current
// addresses (after a standby is added or removed).
func (s *Server) broadcastManagers() {
	term, proof, addrs := s.failoverView()
	payload := protocol.ManagersPayload{Term: term, TermProof: proof, Managers: addrs}
	for _, rec := range s.Registry.List() {
		if rec.State != domain.NodeReady || !rec.hasAgentFeature(domain.FeatureFailover) {
			continue
		}
		conn := s.Registry.ConnOf(rec.Node.Identity.NodeID)
		if conn == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.send(ctx, conn, protocol.MsgManagers, domain.ManagerNodeID, rec.Node.Identity.NodeID, payload)
		cancel()
	}
}

// StandbyHandler serves the standby link on the agent-facing listener
// (cmd/manager mounts it at /standby/ on the direct TLS transport only).
func (s *Server) StandbyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// Never over plaintext: the link carries the manager's private key.
		if r.TLS == nil {
			http.Error(w, "the standby link needs TLS", http.StatusForbidden)
			return
		}
		fs, ok := s.failoverStore()
		if !ok {
			http.NotFound(w, r)
			return
		}
		path := r.URL.Path
		if r.Method == http.MethodPost && path == failover.RouteEnroll {
			s.standbyEnroll(w, r, fs)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pair, ok := s.standbyAuthorized(r)
		if !ok {
			s.auditRejection("standby.rejected", "", r.RemoteAddr, "invalid standby credential", map[string]any{"path": clip(path)})
			http.Error(w, "not this manager's standby", http.StatusUnauthorized)
			return
		}
		switch {
		case path == failover.RouteSync:
			s.standbySync(w, r, fs, pair)
		case path == failover.RouteStatus:
			f := s.failover
			f.mu.Lock()
			st := failover.Status{Role: failover.RoleActive, Term: f.term, TermProof: f.proof}
			if f.fenced {
				st = failover.Status{Role: failover.RoleStandby, Term: f.term}
			}
			f.mu.Unlock()
			writeJSON(w, http.StatusOK, st)
		case strings.HasPrefix(path, failover.RouteArtifacts):
			s.standbyArtifact(w, r, strings.TrimPrefix(path, failover.RouteArtifacts))
		default:
			http.NotFound(w, r)
		}
	}
}

// standbyAuthorized checks the pair secret (constant time).
func (s *Server) standbyAuthorized(r *http.Request) (domain.StandbyPair, bool) {
	got := []byte(bearerToken(r))
	f := s.failover
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pair == nil || len(got) == 0 || subtle.ConstantTimeCompare(got, []byte(f.pair.Secret)) != 1 {
		return domain.StandbyPair{}, false
	}
	return *f.pair, true
}

// standbyEnroll admits a new standby: the one-time token (single use,
// 10 minutes) buys the pair secret. A second standby replaces the first,
// whose secret stops working: one standby at a time, or two could be
// promoted to the same term.
func (s *Server) standbyEnroll(w http.ResponseWriter, r *http.Request, fs failoverStore) {
	token := bearerToken(r)
	sum := sha256.Sum256([]byte(token))
	f := s.failover
	f.mu.Lock()
	valid := token != "" && time.Now().Before(f.enrollUntil) && subtle.ConstantTimeCompare(sum[:], f.enrollHash[:]) == 1
	if valid {
		f.enrollHash, f.enrollUntil = [32]byte{}, time.Time{}
	}
	f.mu.Unlock()
	if !valid {
		s.auditRejection("standby.rejected", "", r.RemoteAddr, "invalid standby enrollment token", nil)
		http.Error(w, "that standby token is not valid (single use, 10 minutes): make a new one with harnessctl standby add", http.StatusUnauthorized)
		return
	}
	var req failover.EnrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Addr != "" {
		if _, port, err := net.SplitHostPort(req.Addr); err != nil || port == "" || len(req.Addr) > 255 {
			http.Error(w, "bad request: addr must be host:port", http.StatusBadRequest)
			return
		}
	}
	secret, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pair := domain.StandbyPair{Secret: secret, PeerAddr: req.Addr, PeerName: clip(req.Name), AddedAt: time.Now().UTC()}
	if err := fs.PutStandbyPair(pair); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	replaced := f.pair != nil
	f.pair = &pair
	f.lastSync, f.lastETag = time.Time{}, ""
	term := f.term
	f.mu.Unlock()
	s.audit(domain.AuditAdmissions, "standby.added", "", actorStandby, map[string]any{
		"name": pair.PeerName, "addr": clip(pair.PeerAddr), "remote": r.RemoteAddr, "replaced": replaced, "by": "standby-enrollment:" + tokenPrefix(token),
	})
	log.Printf("manager: standby added: %s at %s (remote %s)", pair.PeerName, pair.PeerAddr, r.RemoteAddr)
	s.publish(domain.EventStandby, "", map[string]any{"event": "added"})
	s.broadcastManagers()
	writeJSON(w, http.StatusOK, failover.EnrollResponse{Secret: secret, Term: term})
}

// standbySync hands the standby the whole state: 304 when nothing changed
// since what it has, else a fresh bundle. The database is snapshotted to
// a local file first, so the read transaction ends before the (possibly
// slow) transfer: a long one would hold up the manager's writes.
func (s *Server) standbySync(w http.ResponseWriter, r *http.Request, fs failoverStore, pair domain.StandbyPair) {
	if v := r.Header.Get(failover.HeaderSteppedDown); v != "" {
		from, _ := strconv.ParseUint(v, 10, 64)
		s.audit(domain.AuditAdmissions, "standby.stepped-down", "", actorStandby, map[string]any{
			"fromTerm": from, "term": s.Term(), "name": pair.PeerName, "addr": clip(pair.PeerAddr), "remote": r.RemoteAddr,
		})
		log.Printf("manager: the old manager at %s stepped down (it was at term %d) and is now this one's standby", pair.PeerAddr, from)
		s.publish(domain.EventStandby, "", map[string]any{"event": "stepped-down", "term": s.Term()})
	}
	shas := s.artifactSHAs()
	version, err := fs.Version()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == s.standbyETag(version, shas) {
		s.noteStandbySync(inm)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(fs.Path()), "standby-snapshot-*.tmp")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	txid, err := fs.Snapshot(tmp)
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	certPEM, keyPEM, perr := mtls.PEM(s.cfg.TLSCert)
	if err = errors.Join(err, perr); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	etag := s.standbyETag(txid, shas)
	st := failover.State{Term: s.Term(), PairingToken: s.cfg.PairingToken, AIKey: s.cfg.AIKey, Artifacts: shas, CreatedAt: time.Now().UTC()}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("ETag", etag)
	if err := failover.WriteBundle(w, st, tmp, certPEM, keyPEM); err != nil {
		log.Printf("manager: send state to the standby at %s: %v", r.RemoteAddr, err)
		return
	}
	s.noteStandbySync(etag)
}

func (s *Server) noteStandbySync(etag string) {
	f := s.failover
	f.mu.Lock()
	first := f.lastSync.IsZero()
	f.lastSync, f.lastETag = time.Now(), etag
	f.mu.Unlock()
	if first {
		s.publish(domain.EventStandby, "", map[string]any{"event": "synced"})
	}
}

// standbyETag names the state a standby has: the database version plus
// a digest of everything sent beside it.
func (s *Server) standbyETag(version uint64, shas []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%s\n%s\n%s\n", s.Term(), s.cfg.PairingToken, s.cfg.AIKey, s.cfg.Fingerprint)
	for _, sha := range shas {
		io.WriteString(h, sha)
	}
	return fmt.Sprintf(`"%d-%x"`, version, h.Sum(nil)[:8])
}

func (s *Server) artifactSHAs() []string {
	if s.cfg.Artifacts == nil {
		return nil
	}
	list, err := s.cfg.Artifacts.List()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, in := range list {
		out = append(out, in.SHA256)
	}
	sort.Strings(out)
	return out
}

func (s *Server) standbyArtifact(w http.ResponseWriter, r *http.Request, sha string) {
	if s.cfg.Artifacts == nil || !domain.ValidSHA256(sha) {
		http.NotFound(w, r)
		return
	}
	f, info, err := s.cfg.Artifacts.Open(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	io.Copy(w, f)
}

// standbyView is GET /standby on the active manager. Never the secret.
type standbyView struct {
	Role string `json:"role"`
	Term uint64 `json:"term"`
	// Available: this manager can have a standby (TLS and a database).
	Available           bool             `json:"available"`
	Reason              string           `json:"reason,omitempty"`
	Standby             *standbyPeerView `json:"standby,omitempty"`
	EnrollmentExpiresAt *time.Time       `json:"enrollmentExpiresAt,omitempty"`
}

type standbyPeerView struct {
	Name    string    `json:"name,omitempty"`
	Addr    string    `json:"addr"`
	AddedAt time.Time `json:"addedAt"`
	// LastSyncAt: the standby last fetched or confirmed the state.
	// LagSeconds: since then (-1: never). UpToDate: what it has is what
	// this manager has now.
	LastSyncAt time.Time `json:"lastSyncAt,omitempty"`
	LagSeconds int64     `json:"lagSeconds"`
	UpToDate   bool      `json:"upToDate"`
}

func (s *Server) standbyStatus() standbyView {
	fs, ok := s.failoverStore()
	f := s.failover
	f.mu.Lock()
	v := standbyView{Role: failover.RoleActive, Term: f.term, Available: ok}
	if f.fenced {
		v.Role = "stepped-down"
	}
	if !ok {
		v.Reason = "a standby needs a manager with TLS and a database"
	}
	if f.pair != nil {
		p := &standbyPeerView{Name: f.pair.PeerName, Addr: f.pair.PeerAddr, AddedAt: f.pair.AddedAt, LastSyncAt: f.lastSync, LagSeconds: -1}
		if !f.lastSync.IsZero() {
			p.LagSeconds = int64(time.Since(f.lastSync).Seconds())
		}
		v.Standby = p
	}
	if time.Now().Before(f.enrollUntil) {
		until := f.enrollUntil
		v.EnrollmentExpiresAt = &until
	}
	lastETag := f.lastETag
	f.mu.Unlock()
	if v.Standby != nil && lastETag != "" && ok {
		if version, err := fs.Version(); err == nil {
			v.Standby.UpToDate = lastETag == s.standbyETag(version, s.artifactSHAs())
		}
	}
	return v
}

// apiGetStandby serves GET /standby.
func (s *Server) apiGetStandby(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.standbyStatus())
}

// apiAddStandby serves POST /standby: a one-time standby enrollment, and
// the command that starts the standby with it.
func (s *Server) apiAddStandby(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.failoverStore(); !ok {
		http.Error(w, "this manager can't have a standby: it needs TLS (not -insecure) and a database", http.StatusConflict)
		return
	}
	token, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	until := time.Now().Add(standbyEnrollmentTTL)
	f := s.failover
	f.mu.Lock()
	f.enrollHash, f.enrollUntil = sha256.Sum256([]byte(token)), until
	f.mu.Unlock()
	primary := s.cfg.AdvertiseAddr
	if primary == "" {
		primary = "<this manager's LAN address>:7420"
	}
	s.audit(domain.AuditSecurity, "standby.enrollment-created", "", actorFrom(r.Context()), map[string]any{"token": tokenPrefix(token), "expiresAt": until.UTC()})
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": token, "fingerprint": s.cfg.Fingerprint, "primary": primary, "expiresAt": until.UTC(),
		"command": fmt.Sprintf("manager -standby-of %s -standby-fingerprint %s -standby-token %s -advertise-addr <the standby's LAN address>:7420", primary, s.cfg.Fingerprint, token),
	})
}

// apiRemoveStandby serves DELETE /standby: forget the standby (its secret
// stops working at once).
func (s *Server) apiRemoveStandby(w http.ResponseWriter, r *http.Request) {
	fs, ok := s.failoverStore()
	if !ok {
		http.Error(w, "this manager has no standby", http.StatusConflict)
		return
	}
	if err := fs.DeleteStandbyPair(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f := s.failover
	f.mu.Lock()
	var was domain.StandbyPair
	if f.pair != nil {
		was = *f.pair
	}
	f.pair, f.enrollUntil, f.lastSync, f.lastETag = nil, time.Time{}, time.Time{}, ""
	f.mu.Unlock()
	s.audit(domain.AuditSecurity, "standby.removed", "", actorFrom(r.Context()), map[string]any{"name": was.PeerName, "addr": was.PeerAddr})
	s.publish(domain.EventStandby, "", map[string]any{"event": "removed"})
	s.broadcastManagers()
	writeJSON(w, http.StatusOK, s.standbyStatus())
}

// apiPromoteActive answers POST /standby/promote on a manager that is
// already the active one.
func (s *Server) apiPromoteActive(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "this manager is the active one: run promote on the standby's machine", http.StatusConflict)
}

// StandbyRole is a manager running as a standby (failover.Replica).
type StandbyRole interface {
	Status() failover.ReplicaStatus
	Promote(ctx context.Context, force bool, actor string) error
}

// StandbyRoleHandler is the operator API of a manager while it is a
// standby: only its status and the promotion, behind the same browser
// guard and operator token as the full API.
func StandbyRoleHandler(role StandbyRole, operatorToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "This manager is a standby: it keeps a copy of the active manager's state and doesn't serve devices.\nOn this machine: harnessctl standby status, or harnessctl standby promote to take over.\n")
	})
	mux.HandleFunc("GET /standby", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, role.Status())
	})
	mux.HandleFunc("POST /standby/promote", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Force bool `json:"force"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		if err := role.Promote(r.Context(), req.Force, actorFrom(r.Context())); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, failover.ErrPromotionRefused) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"promoted": true, "term": role.Status().Term})
	})
	auth := &Server{cfg: Config{OperatorToken: operatorToken}}
	return guardOperatorAPI(auth.requireOperator(mux))
}
