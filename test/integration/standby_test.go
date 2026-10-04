package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/failover"
	"home-harness/internal/identity"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	"home-harness/internal/protocol"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// Standby manager and failover (roadmap item 17). Each standbyHost is one
// manager machine: a state directory, run either as the active manager —
// composed exactly like cmd/manager: pinned TLS, bbolt store, the standby
// link on the agent listener, StepDown writing the role file — or through
// failover.Phase, as cmd/manager's standby phase does.

type standbyHost struct {
	t         *testing.T
	name      string
	addr      string
	dir       string
	peerCheck time.Duration

	srv     *manager.Server
	store   *persistent.Store
	apiSrv  *httptest.Server
	cancel  context.CancelFunc
	done    chan struct{}
	stepped chan struct{}
	fp      string

	standbyAPI atomic.Value // string: the operator API URL while a standby
}

func newStandbyHost(t *testing.T, name, addr string) *standbyHost {
	return &standbyHost{t: t, name: name, addr: addr, dir: filepath.Join(t.TempDir(), name), peerCheck: time.Hour}
}

func (h *standbyHost) db() string     { return filepath.Join(h.dir, "manager.db") }
func (h *standbyHost) tlsDir() string { return filepath.Join(h.dir, "tls") }
func (h *standbyHost) aiKey() string  { return filepath.Join(h.dir, "ai-key") }
func (h *standbyHost) api() string    { return h.apiSrv.URL }

// startActive runs the host as an active manager from its state dir.
func (h *standbyHost) startActive(pairing string) {
	t := h.t
	t.Helper()
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := persistent.Open(h.db())
	if err != nil {
		t.Fatalf("%s: open store: %v", h.name, err)
	}
	cert, err := mtls.LoadOrCreateCert(h.tlsDir())
	if err != nil {
		t.Fatalf("%s: cert: %v", h.name, err)
	}
	aiKey, err := manager.LoadOrCreateOperatorToken(h.aiKey())
	if err != nil {
		t.Fatal(err)
	}
	h.fp = mtls.Fingerprint(cert)
	transport := ws.NewTLSServer(cert)
	ctx, cancel := context.WithCancel(context.Background())
	stepped := make(chan struct{})
	var once sync.Once
	srv := manager.NewServer(transport, store, manager.Config{
		Addr: h.addr, PairingToken: pairing, HeartbeatTimeout: 5 * time.Second, ReconcileInterval: 100 * time.Millisecond,
		Fingerprint: h.fp, TLSCert: cert, AdvertiseAddr: h.addr, AIKey: aiKey, PeerCheckInterval: h.peerCheck,
		StepDown: func(pair domain.StandbyPair, own, term uint64) {
			if _, err := failover.StepDown(h.db(), h.fp, pair, own, term); err != nil {
				t.Errorf("%s: step down: %v", h.name, err)
			}
			once.Do(func() { close(stepped) })
			cancel()
		},
	})
	transport.Handle("/standby/", srv.StandbyHandler())
	done := make(chan struct{})
	go func() {
		srv.Run(ctx)
		close(done)
	}()
	waitListening(t, h.addr)
	h.srv, h.store, h.cancel, h.done, h.stepped = srv, store, cancel, done, stepped
	h.apiSrv = httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(h.stop)
}

// stop shuts the active manager down (like killing it) and closes its store.
func (h *standbyHost) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Errorf("%s: manager didn't stop", h.name)
	}
	h.apiSrv.Close()
	// Let connection goroutines finish their last writes before closing.
	time.Sleep(200 * time.Millisecond)
	h.store.Close()
	h.cancel = nil
}

type phaseResult struct {
	out failover.Outcome
	err error
}

// runPhase runs failover.Phase for this host's state, like cmd/manager's
// standby phase; the result arrives when it ends.
func (h *standbyHost) runPhase(ctx context.Context, opt failover.PhaseOptions) <-chan phaseResult {
	opt.DBPath, opt.TLSDir, opt.AIKeyFile, opt.Addr, opt.Name = h.db(), h.tlsDir(), h.aiKey(), h.addr, h.name
	opt.API = func(r *failover.Replica) func() {
		s := httptest.NewServer(manager.StandbyRoleHandler(r, ""))
		h.standbyAPI.Store(s.URL)
		return s.Close
	}
	ch := make(chan phaseResult, 1)
	go func() {
		out, err := failover.Phase(ctx, opt)
		ch <- phaseResult{out, err}
	}()
	return ch
}

func (h *standbyHost) replicaStatus() (failover.ReplicaStatus, bool) {
	url, _ := h.standbyAPI.Load().(string)
	if url == "" {
		return failover.ReplicaStatus{}, false
	}
	resp, err := http.Get(url + "/standby")
	if err != nil {
		return failover.ReplicaStatus{}, false
	}
	defer resp.Body.Close()
	var st failover.ReplicaStatus
	return st, json.NewDecoder(resp.Body).Decode(&st) == nil
}

func (h *standbyHost) promote(force bool) (int, string) {
	url, _ := h.standbyAPI.Load().(string)
	body := `{"force":false}`
	if force {
		body = `{"force":true}`
	}
	resp, err := http.Post(url+"/standby/promote", "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("promote: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type activeStandbyView struct {
	Role    string `json:"role"`
	Term    uint64 `json:"term"`
	Standby *struct {
		Addr       string    `json:"addr"`
		LastSyncAt time.Time `json:"lastSyncAt"`
		LagSeconds int64     `json:"lagSeconds"`
		UpToDate   bool      `json:"upToDate"`
	} `json:"standby"`
}

func (h *standbyHost) activeStandby() activeStandbyView {
	var v activeStandbyView
	getJSON(h.t, h.api()+"/standby", &v)
	return v
}

// addStandby makes a one-time enrollment on the active host and starts
// sb as its standby; it returns once sb holds a copy the primary
// confirms is current.
func addStandby(t *testing.T, ctx context.Context, primary, sb *standbyHost, opt failover.PhaseOptions) <-chan phaseResult {
	t.Helper()
	resp, err := http.Post(primary.api()+"/standby", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var enr struct {
		Token, Fingerprint, Primary string
	}
	json.NewDecoder(resp.Body).Decode(&enr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || enr.Token == "" || enr.Fingerprint != primary.fp || enr.Primary != primary.addr {
		t.Fatalf("POST /standby: %d %+v", resp.StatusCode, enr)
	}
	opt.Primary, opt.Token, opt.Fingerprint = primary.addr, enr.Token, enr.Fingerprint
	if opt.Interval == 0 {
		opt.Interval = 200 * time.Millisecond
	}
	ch := sb.runPhase(ctx, opt)
	waitFor(t, 15*time.Second, func() bool {
		st, ok := sb.replicaStatus()
		v := primary.activeStandby()
		return ok && st.Copied && v.Standby != nil && v.Standby.UpToDate
	})
	return ch
}

func startFailoverAgent(t *testing.T, ctx context.Context, addr, fp, name string, follow bool) (*agent.Agent, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	a, err := agent.New(ws.NewTLSClient(mtls.PinnedClientConfig(fp)), agent.Config{
		DeviceUse: pluggedIn, OllamaURL: "127.0.0.1:1", ManagerAddr: addr, PairingToken: pairingToken,
		IdentityDir: dir, WorkDir: filepath.Join(t.TempDir(), name+"-work"), Name: name,
		HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 300 * time.Millisecond,
		HostFingerprint: "-", ManagerFingerprint: fp, Failover: follow,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a, dir
}

type agentKnown struct {
	Fingerprint string   `json:"fingerprint"`
	Term        uint64   `json:"term"`
	Managers    []string `json:"managers"`
}

func readAgentKnown(dir string) agentKnown {
	var k agentKnown
	data, err := os.ReadFile(filepath.Join(dir, "managers.json"))
	if err == nil {
		json.Unmarshal(data, &k)
	}
	return k
}

func readyOn(srv *manager.Server, id domain.NodeID) bool {
	rec, ok := srv.Registry.Get(id)
	return ok && rec.State == domain.NodeReady
}

func runIdentity(t *testing.T, srv *manager.Server) domain.WorkloadID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := srv.Submit(ctx, manager.WorkloadSpec{Capability: "system.identity"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return workloadState(srv, w.ID) == domain.WorkloadCompleted })
	return w.ID
}

func auditKinds(t *testing.T, api string) string {
	var entries []domain.AuditEntry
	getJSON(t, api+"/audit?limit=200", &entries)
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	return strings.Join(kinds, ",")
}

// The whole story: a standby copies the primary and keeps up; refuses to
// take over while the primary answers; the primary dies; the standby is
// promoted; the devices — pinned to the same identity — reconnect to it on
// their own, interrupted work shows UNKNOWN, new work runs; the old
// primary comes back, finds it was superseded before serving anyone, and
// becomes the new manager's standby. An agent without failover.v1 keeps
// working against the primary only.
func TestStandbyFailoverEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newStandbyHost(t, "primary", "127.0.0.1:19680")
	p.startActive(pairingToken)
	a1, dir1 := startFailoverAgent(t, ctx, p.addr, p.fp, "fo-a1", true)
	a2, dir2 := startFailoverAgent(t, ctx, p.addr, p.fp, "fo-a2", true)
	old, oldDir := startFailoverAgent(t, ctx, p.addr, p.fp, "fo-old", false)
	waitFor(t, 10*time.Second, func() bool {
		return readyOn(p.srv, a1.NodeID()) && readyOn(p.srv, a2.NodeID()) && readyOn(p.srv, old.NodeID())
	})
	before := runIdentity(t, p.srv)

	s := newStandbyHost(t, "standby", "127.0.0.1:19681")
	phase := addStandby(t, ctx, p, s, failover.PhaseOptions{})

	// Connected failover.v1 agents are told where the standby is; the old
	// agent isn't told anything.
	waitFor(t, 5*time.Second, func() bool {
		k1, k2 := readAgentKnown(dir1), readAgentKnown(dir2)
		return strings.Join(k1.Managers, ",") == p.addr+","+s.addr && strings.Join(k2.Managers, ",") == p.addr+","+s.addr
	})
	if k := readAgentKnown(oldDir); len(k.Managers) != 0 {
		t.Fatalf("an agent without failover.v1 was told managers: %+v", k)
	}
	// The standby doesn't serve devices: nothing listens on its address.
	if c, err := net.DialTimeout("tcp", s.addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("the standby listens for devices while the primary is alive")
	}
	// It refuses to take over while the primary answers.
	if code, body := s.promote(false); code != http.StatusConflict || !strings.Contains(body, "still answers") {
		t.Fatalf("promote with the primary alive: %d %s", code, body)
	}

	// Work in flight at the failover, and replication catching up with it.
	sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
	burn, err := p.srv.Submit(sctx, manager.WorkloadSpec{Capability: "cpu.burn", Params: map[string]string{"seconds": "60", "threads": "1"}})
	scancel()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return workloadState(p.srv, burn.ID) == domain.WorkloadRunning })
	waitFor(t, 10*time.Second, func() bool {
		v := p.activeStandby()
		return v.Standby != nil && v.Standby.UpToDate
	})

	// The primary dies.
	p.stop()
	if code, body := s.promote(false); code != http.StatusOK {
		t.Fatalf("promote: %d %s", code, body)
	}
	var res phaseResult
	select {
	case res = <-phase:
	case <-time.After(10 * time.Second):
		t.Fatal("the standby phase didn't end after promotion")
	}
	if res.err != nil || !res.out.Promoted || res.out.PairingToken != pairingToken {
		t.Fatalf("phase outcome: %+v", res)
	}
	s.startActive(res.out.PairingToken)
	if s.fp != p.fp {
		t.Fatalf("the standby serves identity %s, not the primary's %s", s.fp, p.fp)
	}
	if got := s.srv.Term(); got != 1 {
		t.Fatalf("promoted term = %d, want 1", got)
	}

	// The devices find it on their own, with the same pin, and learn the term.
	waitFor(t, 15*time.Second, func() bool { return readyOn(s.srv, a1.NodeID()) && readyOn(s.srv, a2.NodeID()) })
	waitFor(t, 5*time.Second, func() bool { return readAgentKnown(dir1).Term == 1 && readAgentKnown(dir2).Term == 1 })
	if rec, ok := s.srv.Workloads.Get(before); !ok || rec.Status.State != domain.WorkloadCompleted {
		t.Fatalf("work finished before the failover was not copied: %+v", rec)
	}
	if st := workloadState(s.srv, burn.ID); st != domain.WorkloadUnknown {
		t.Fatalf("work running at the failover is %s on the new manager, want UNKNOWN", st)
	}
	runIdentity(t, s.srv) // work continues
	// The agent without failover.v1 stays with the primary's address.
	time.Sleep(time.Second)
	if readyOn(s.srv, old.NodeID()) {
		t.Fatal("an agent without failover.v1 followed the standby")
	}

	// The old primary comes back: it asks its standby first, learns it took
	// over, and becomes its standby without ever listening for devices.
	p2 := newStandbyHost(t, "primary", p.addr)
	p2.dir = p.dir
	phase2 := p2.runPhase(ctx, failover.PhaseOptions{Options: failover.Options{Interval: 200 * time.Millisecond}})
	waitFor(t, 15*time.Second, func() bool {
		st, ok := p2.replicaStatus()
		return ok && st.Primary == s.addr && st.Copied && st.Term == 1
	})
	select {
	case r := <-phase2:
		t.Fatalf("the old primary's phase ended: %+v", r)
	default:
	}
	if c, err := net.DialTimeout("tcp", p.addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("the stepped-down primary listens for devices")
	}
	waitFor(t, 5*time.Second, func() bool {
		v := s.activeStandby()
		return v.Role == "active" && v.Term == 1 && v.Standby != nil && v.Standby.Addr == p.addr && v.Standby.UpToDate
	})
	kinds := auditKinds(t, s.api())
	for _, want := range []string{"standby.promoted", "standby.stepped-down", "standby.added", "standby.enrollment-created"} {
		if !strings.Contains(kinds, want) {
			t.Errorf("audit log lacks %s: %s", want, kinds)
		}
	}
	if !readyOn(s.srv, a1.NodeID()) || !readyOn(s.srv, a2.NodeID()) {
		t.Fatal("devices left the active manager")
	}
}

// A standby force-promoted while the primary still runs (a planned
// switch-over, or a primary that was asleep): the primary sees it at its
// next peer check, steps down, and the device moves.
func TestStandbyPeerCheckStepsDownLivePrimary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newStandbyHost(t, "primary", "127.0.0.1:19682")
	p.peerCheck = 200 * time.Millisecond
	p.startActive(pairingToken)
	a, dir := startFailoverAgent(t, ctx, p.addr, p.fp, "fo-live", true)
	waitFor(t, 10*time.Second, func() bool { return readyOn(p.srv, a.NodeID()) })

	s := newStandbyHost(t, "standby", "127.0.0.1:19683")
	phase := addStandby(t, ctx, p, s, failover.PhaseOptions{})
	waitFor(t, 5*time.Second, func() bool { return len(readAgentKnown(dir).Managers) == 2 })

	if code, body := s.promote(true); code != http.StatusOK {
		t.Fatalf("forced promote: %d %s", code, body)
	}
	res := <-phase
	if !res.out.Promoted {
		t.Fatalf("not promoted: %+v", res)
	}
	s.startActive(res.out.PairingToken)

	select {
	case <-p.stepped:
	case <-time.After(10 * time.Second):
		t.Fatal("the live primary didn't step down")
	}
	if !p.srv.SteppedDown() {
		t.Fatal("SteppedDown() is false")
	}
	role, err := failover.LoadRole(p.db())
	if err != nil || role == nil || role.Role != failover.RoleStandby || role.Primary != s.addr || role.SeenTerm != 1 {
		t.Fatalf("stepped-down role file: %+v %v", role, err)
	}
	waitFor(t, 15*time.Second, func() bool { return readyOn(s.srv, a.NodeID()) })
	runIdentity(t, s.srv)
}

// The other fence: the old primary keeps running and never checks its
// peer, but a device that has been with the newer manager registers with
// it, carrying the signed term — it steps down and turns the device away.
// A made-up or foreign-signed term does nothing.
func TestStandbyAgentTermStepsDownStalePrimary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newStandbyHost(t, "primary", "127.0.0.1:19684")
	p.startActive(pairingToken) // peer check effectively off (1 h)
	a, _ := startFailoverAgent(t, ctx, p.addr, p.fp, "fo-stale-a", true)
	waitFor(t, 10*time.Second, func() bool { return readyOn(p.srv, a.NodeID()) })

	s := newStandbyHost(t, "standby", "127.0.0.1:19685")
	phase := addStandby(t, ctx, p, s, failover.PhaseOptions{})
	if code, body := s.promote(true); code != http.StatusOK {
		t.Fatalf("forced promote: %d %s", code, body)
	}
	res := <-phase
	s.startActive(res.out.PairingToken)

	// Forged terms: garbage, and a real signature by another key.
	other, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := failover.SignTerm(other, 7)
	for _, proof := range [][]byte{[]byte("not a signature"), foreign} {
		env := rawTLSRegister(t, p.addr, p.fp, 7, proof)
		if env.Type != protocol.MsgRegisterAck {
			t.Fatalf("a forged term was refused instead of ignored: %s", env.Type)
		}
	}
	if p.srv.SteppedDown() {
		t.Fatal("a forged term made the primary step down")
	}

	// A device that knows the newer manager: joins it, then (it gone) tries
	// the old primary it was told about.
	b, bdir := startFailoverAgent(t, ctx, s.addr, s.fp, "fo-stale-b", true)
	waitFor(t, 10*time.Second, func() bool { return readyOn(s.srv, b.NodeID()) && readAgentKnown(bdir).Term == 1 })
	if k := readAgentKnown(bdir); strings.Join(k.Managers, ",") != s.addr+","+p.addr {
		t.Fatalf("managers told by the promoted manager: %v", k.Managers)
	}
	s.stop()
	select {
	case <-p.stepped:
	case <-time.After(15 * time.Second):
		t.Fatal("the stale primary didn't step down when a device brought the newer term")
	}
	if readyOn(p.srv, b.NodeID()) {
		t.Fatal("the stale primary admitted the device")
	}
}

// rawTLSRegister registers a fresh identity over pinned TLS with a given
// term and proof, returning the manager's answer.
func rawTLSRegister(t *testing.T, addr, fp string, term uint64, proof []byte) *protocol.Envelope {
	t.Helper()
	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := ws.NewTLSClient(mtls.PinnedClientConfig(fp)).Dial(ctx, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	payload := protocol.RegisterPayload{
		Manifest:     domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Node: domain.Node{Identity: id.Identity, Name: "raw"}},
		PairingToken: pairingToken, Signature: id.Sign(protocol.RegisterSignedData(pairingToken, id.NodeID)), Term: term, TermProof: proof,
	}
	env, _ := protocol.NewEnvelope(protocol.MsgRegister, id.NodeID, domain.ManagerNodeID, payload)
	wire, _ := protocol.Encode(env)
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatal(err)
	}
	data, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	resp, err := protocol.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The standby link refuses everything without its credential: no token, a
// wrong token, a spent one-time token, a wrong pair secret, plaintext, and
// a client that pins another identity. The operator API never shows the
// secret.
func TestStandbyLinkAuthentication(t *testing.T) {
	p := newStandbyHost(t, "primary", "127.0.0.1:19686")
	p.startActive(pairingToken)
	client := failover.NewClient(p.fp)
	call := func(method, route, bearer, body string) (int, []byte) {
		req, _ := http.NewRequest(method, "https://"+p.addr+route, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, route, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if code, _ := call("POST", failover.RouteEnroll, "", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("enroll without a token: %d", code)
	}
	if code, _ := call("POST", failover.RouteEnroll, strings.Repeat("ab", 32), `{}`); code != http.StatusUnauthorized {
		t.Fatalf("enroll with a made-up token: %d", code)
	}
	resp, err := http.Post(p.api()+"/standby", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var enr struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&enr)
	resp.Body.Close()
	code, body := call("POST", failover.RouteEnroll, enr.Token, `{"name":"sb","addr":"127.0.0.1:19687"}`)
	var got failover.EnrollResponse
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || len(got.Secret) < 32 {
		t.Fatalf("enroll: %d %s", code, body)
	}
	if code, _ := call("POST", failover.RouteEnroll, enr.Token, `{}`); code != http.StatusUnauthorized {
		t.Fatalf("a spent enrollment token worked again: %d", code)
	}
	for _, route := range []string{failover.RouteSync, failover.RouteStatus, failover.RouteArtifacts + strings.Repeat("0", 64)} {
		if code, _ := call("GET", route, "", ""); code != http.StatusUnauthorized {
			t.Fatalf("%s without the secret: %d", route, code)
		}
		if code, _ := call("GET", route, got.Secret+"x", ""); code != http.StatusUnauthorized {
			t.Fatalf("%s with a wrong secret: %d", route, code)
		}
	}
	code, body = call("GET", failover.RouteSync, got.Secret, "")
	if code != http.StatusOK || !bytes.HasPrefix(body, []byte("PK")) {
		t.Fatalf("sync with the secret: %d", code)
	}
	if code, _ := call("GET", failover.RouteStatus, got.Secret, ""); code != http.StatusOK {
		t.Fatalf("status with the secret: %d", code)
	}

	// Plaintext: the same handler behind a plain listener refuses.
	plain := httptest.NewServer(p.srv.StandbyHandler())
	defer plain.Close()
	req, _ := http.NewRequest("GET", plain.URL+failover.RouteSync, nil)
	req.Header.Set("Authorization", "Bearer "+got.Secret)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sync over plaintext: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// A client pinned to another identity never gets an answer.
	other, _ := mtls.LoadOrCreateCert(t.TempDir())
	if _, err := failover.FetchStatus(context.Background(), failover.NewClient(mtls.Fingerprint(other)), p.addr, got.Secret); err == nil {
		t.Fatal("a client pinned to another identity got a status")
	}
	// The pinned one does, with a proof only for a term above 0.
	st, err := failover.FetchStatus(context.Background(), client, p.addr, got.Secret)
	if err != nil || st.Role != failover.RoleActive || st.Term != 0 {
		t.Fatalf("status: %+v %v", st, err)
	}

	// The operator API shows the standby, never its secret.
	resp, err = http.Get(p.api() + "/standby")
	if err != nil {
		t.Fatal(err)
	}
	view, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(view, []byte(got.Secret)) || !bytes.Contains(view, []byte("127.0.0.1:19687")) {
		t.Fatalf("GET /standby: %s", view)
	}
}

// Automatic promotion (off by default): the standby takes over by itself
// once the primary hasn't answered for the configured time.
func TestStandbyAutoPromotes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newStandbyHost(t, "primary", "127.0.0.1:19688")
	p.startActive(pairingToken)
	a, dir := startFailoverAgent(t, ctx, p.addr, p.fp, "fo-auto", true)
	waitFor(t, 10*time.Second, func() bool { return readyOn(p.srv, a.NodeID()) })
	s := newStandbyHost(t, "standby", "127.0.0.1:19689")
	phase := addStandby(t, ctx, p, s, failover.PhaseOptions{Options: failover.Options{AutoPromoteAfter: time.Second}})
	waitFor(t, 5*time.Second, func() bool { return len(readAgentKnown(dir).Managers) == 2 })
	time.Sleep(2 * time.Second) // still answering: no promotion
	select {
	case r := <-phase:
		t.Fatalf("promoted while the primary answers: %+v", r)
	default:
	}
	p.stop()
	var res phaseResult
	select {
	case res = <-phase:
	case <-time.After(15 * time.Second):
		t.Fatal("the standby didn't promote itself")
	}
	if !res.out.Promoted {
		t.Fatalf("phase: %+v", res)
	}
	s.startActive(res.out.PairingToken)
	if !strings.Contains(auditKinds(t, s.api()), "standby.promoted") {
		t.Fatal("no standby.promoted audit entry")
	}
	waitFor(t, 15*time.Second, func() bool { return readyOn(s.srv, a.NodeID()) })
}
