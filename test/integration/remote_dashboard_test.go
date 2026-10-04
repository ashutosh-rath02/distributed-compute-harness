// Remote dashboard integration tests (roadmap item 18): an in-process
// relay (rendezvous + public gateway), a manager reachable only through
// it, an agent connected through it, and a Go client standing in for the
// remote browser (remoteaccess.Client speaks exactly what remote.js
// speaks). Between the client and the gateway sits evilRelay: it records
// everything the relay operator would see and, on demand, replays,
// changes or forges traffic, which must all be refused.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	relayproto "home-harness/internal/relay"
	"home-harness/internal/remoteaccess"
	relaytransport "home-harness/internal/transport/relay"
)

const (
	remoteTestOperatorToken = "remote-test-operator-token-0123456789abcdef0123456789abcdef"
	remoteTestRelaySession  = "remote-test-private-relay-session"
)

type remoteFixture struct {
	srv      *manager.Server
	operator http.Handler
	gateway  string // the honest gateway's base URL
	evil     *evilRelay
	evilURL  string
	nodeID   domain.NodeID
}

// listenOn binds one of this test's fixed loopback ports.
func listenOn(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	return ln
}

func serveOn(t *testing.T, addr string, h http.Handler) string {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.Listener.Close()
	s.Listener = listenOn(t, addr)
	s.Start()
	t.Cleanup(s.Close)
	return s.URL
}

func startRemoteFixture(t *testing.T, relayAddr, gatewayAddr, evilAddr string) *remoteFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	relayServer := relayproto.NewServerWithAliasKey(5*time.Second, "remote-test-alias-key")
	go relayServer.Serve(ctx, listenOn(t, relayAddr))
	gateway := serveOn(t, gatewayAddr, relayproto.NewPublicGateway(relayServer, relayAddr, "publish-secret").Handler())
	evil := &evilRelay{target: gateway, client: &http.Transport{}}
	t.Cleanup(evil.client.CloseIdleConnections)
	evilURL := serveOn(t, evilAddr, evil)

	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := mtls.Fingerprint(cert)
	managerTransport := relaytransport.NewTLSServer(remoteTestRelaySession, cert)
	srv := manager.NewServer(managerTransport, nil, manager.Config{
		Addr: relayAddr, PairingToken: pairingToken, HeartbeatTimeout: 3 * time.Second,
		Fingerprint: fingerprint, RelayAddr: relayAddr, RelayToken: remoteTestRelaySession, RelayPublicURL: gateway,
		EnrollmentPublisher: &manager.HTTPEnrollmentPublisher{URL: gateway, RelaySession: remoteTestRelaySession, PublishToken: "publish-secret"},
		OperatorToken:       remoteTestOperatorToken,
	})
	managerTransport.Handle("/remote/", srv.RemoteHandler())
	go srv.Run(ctx)

	a, err := agent.New(relaytransport.NewTLSClient(remoteTestRelaySession, mtls.PinnedClientConfig(fingerprint)), agent.Config{
		DeviceUse: pluggedIn, ManagerAddr: relayAddr, PairingToken: pairingToken,
		IdentityDir: filepath.Join(t.TempDir(), "remote-test-agent"), Name: "remote-test-agent",
		HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, WorkloadSlots: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	waitFor(t, 10*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	return &remoteFixture{srv: srv, operator: srv.NewHTTPHandler(), gateway: gateway, evil: evil, evilURL: evilURL, nodeID: a.NodeID()}
}

// operatorDo calls the local operator API with the operator token.
func (f *remoteFixture) operatorDo(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7421"
	req.Header.Set("Authorization", "Bearer "+remoteTestOperatorToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.operator.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (f *remoteFixture) setRemote(t *testing.T, enabled bool, mode string) manager.RemoteAccessView {
	t.Helper()
	code, body := f.operatorDo(t, "PUT", "/remote-access", fmt.Sprintf(`{"enabled":%v,"mode":%q}`, enabled, mode))
	var v manager.RemoteAccessView
	if code != http.StatusOK || json.Unmarshal(body, &v) != nil {
		t.Fatalf("PUT /remote-access: %d %s", code, body)
	}
	return v
}

// client is a remote browser that goes through the (recording) relay.
func (f *remoteFixture) client(v manager.RemoteAccessView) *remoteaccess.Client {
	return &remoteaccess.Client{BaseURL: strings.Replace(v.URL, f.gateway, f.evilURL, 1), Key: v.Key}
}

func (f *remoteFixture) auditCount(t *testing.T, log, kind, actor string) int {
	t.Helper()
	_, body := f.operatorDo(t, "GET", "/audit?limit=1000&log="+log, "")
	var entries []domain.AuditEntry
	json.Unmarshal(body, &entries)
	n := 0
	for _, e := range entries {
		if e.Kind == kind && (actor == "" || e.Actor == actor) {
			n++
		}
	}
	return n
}

// remoteCall makes one call and reads the whole reply; errors (refusals
// before the seal, verification failures) come back as err.
func remoteCall(c *remoteaccess.Client, method, path, body string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, "http://manager"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.RoundTrip(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

func mustRemote(t *testing.T, c *remoteaccess.Client, method, path, body string) (int, string) {
	t.Helper()
	code, out, err := remoteCall(c, method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, out
}

func TestRemoteDashboardThroughRelay(t *testing.T) {
	f := startRemoteFixture(t, "127.0.0.1:19690", "127.0.0.1:19691", "127.0.0.1:19692")

	// Off by default: nothing answers but "it's off".
	_, body := f.operatorDo(t, "GET", "/remote-access", "")
	var initial manager.RemoteAccessView
	json.Unmarshal(body, &initial)
	if initial.Enabled || !initial.Available || initial.URL != "" {
		t.Fatalf("initial remote access: %+v", initial)
	}
	v := f.setRemote(t, true, "standard")
	if !strings.HasPrefix(v.URL, f.gateway+"/r/hr1.") || v.Key == "" {
		t.Fatalf("on: %+v", v)
	}
	f.setRemote(t, false, "")
	page, err := http.Get(v.URL)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusNotFound || !strings.Contains(string(text), "off") {
		t.Fatalf("page while off: %d %s", page.StatusCode, text)
	}
	if again := f.setRemote(t, true, "standard"); again.URL != v.URL || again.Key != v.Key {
		t.Fatal("turning remote access back on changed its address or key")
	}

	// The page, through the relay, with its CSP intact.
	page, err = http.Get(strings.Replace(v.URL, f.gateway, f.evilURL, 1))
	if err != nil {
		t.Fatal(err)
	}
	text, _ = io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(text), "window.harnessRemote") ||
		!strings.Contains(page.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("remote page: %d, CSP %q", page.StatusCode, page.Header.Get("Content-Security-Policy"))
	}

	c := f.client(v)
	if code, out := mustRemote(t, c, "GET", "/nodes", ""); code != http.StatusOK || !strings.Contains(out, string(f.nodeID)) {
		t.Fatalf("GET /nodes: %d %s", code, out)
	}

	// The live event stream, sealed frame by frame, through the relay.
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	req, _ := http.NewRequestWithContext(streamCtx, "GET", "http://manager/events", nil)
	stream, err := c.RoundTrip(req)
	if err != nil || stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET /events: %v", err)
	}
	events := make(chan string, 64)
	streamErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stream.Body.Read(buf)
			if n > 0 {
				events <- string(buf[:n])
			}
			if err != nil {
				streamErr <- err
				return
			}
		}
	}()
	if code, out := mustRemote(t, c, "POST", "/nodes/"+string(f.nodeID)+"/commands", `{"name":"PING"}`); code != http.StatusOK {
		t.Fatalf("remote PING: %d %s", code, out)
	}
	waitEvent := func(want string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case e := <-events:
				if strings.Contains(e, want) {
					return
				}
			case err := <-streamErr:
				t.Fatalf("event stream ended: %v", err)
			case <-deadline:
				t.Fatalf("no %q event through the relay", want)
			}
		}
	}
	waitEvent("command.completed")

	// Run a task remotely; the security log says who.
	code, out := mustRemote(t, c, "POST", "/workloads", `{"capability":"cpu.burn","params":{"seconds":"1"}}`)
	var created struct{ ID string }
	if code != http.StatusAccepted || json.Unmarshal([]byte(out), &created) != nil {
		t.Fatalf("remote task: %d %s", code, out)
	}
	waitFor(t, 20*time.Second, func() bool {
		_, out := mustRemote(t, c, "GET", "/workloads/"+created.ID, "")
		return strings.Contains(out, `"COMPLETED"`)
	})
	if n := f.auditCount(t, "security", "workload.submitted", "remote"); n != 1 {
		t.Fatalf("workload.submitted by remote: %d entries", n)
	}

	// The mode is the manager's to enforce, live.
	if code, out := mustRemote(t, c, "PUT", "/policy", `{"allowUnlisted":true}`); code != http.StatusForbidden || !strings.Contains(out, "full control") {
		t.Fatalf("standard PUT /policy: %d %s", code, out)
	}
	f.setRemote(t, true, "read-only")
	if code, _ := mustRemote(t, c, "POST", "/workloads", `{"capability":"cpu.burn","params":{"seconds":"1"}}`); code != http.StatusForbidden {
		t.Fatalf("read-only task: %d", code)
	}
	f.setRemote(t, true, "full")
	if code, out := mustRemote(t, c, "POST", "/join-window", `{"minutes":5}`); code == http.StatusForbidden && strings.Contains(out, "remote") {
		t.Fatalf("full POST /join-window: %d %s", code, out)
	}
	for _, path := range []string{"/join-info", "/join-script?addr=1.2.3.4:7420", "/ai-info", "/remote-access"} {
		if code, out := mustRemote(t, c, "GET", path, ""); code != http.StatusForbidden || strings.Contains(out, pairingToken) || strings.Contains(out, v.Key) {
			t.Fatalf("full GET %s: %d %s", path, code, out)
		}
	}

	// What the relay saw: neither the key nor any other credential, nor
	// what was asked or answered.
	seen := f.evil.seenBytes()
	norm, _ := remoteaccess.NormalizeKey(v.Key)
	for _, secret := range []string{v.Key, norm, remoteTestOperatorToken, pairingToken, remoteTestRelaySession,
		"/nodes", "/workloads", "cpu.burn", "PING", "/policy", "join-info", string(f.nodeID), "remote-test-agent"} {
		if bytes.Contains(seen, []byte(secret)) {
			t.Errorf("the relay saw %q", secret)
		}
	}
	if len(seen) < 1000 {
		t.Fatalf("the recorder saw only %d bytes: is traffic going through it?", len(seen))
	}
	if n := f.auditCount(t, "remote", "remote.request", "remote"); n < 10 {
		t.Fatalf("remote log: %d requests", n)
	}

	// Turning it off ends open streams (the manager seals their end) and
	// every session.
	f.setRemote(t, false, "")
	select {
	case <-streamErr:
	case <-time.After(10 * time.Second):
		t.Fatal("the event stream outlived turning remote access off")
	}
	if _, _, err := remoteCall(c, "GET", "/nodes", ""); err == nil {
		t.Fatal("a call worked after remote access was turned off")
	}
}

func TestRemoteDashboardRefusesTamperingRelay(t *testing.T) {
	f := startRemoteFixture(t, "127.0.0.1:19693", "127.0.0.1:19694", "127.0.0.1:19695")
	v := f.setRemote(t, true, "standard")
	c := f.client(v)
	if code, _ := mustRemote(t, c, "GET", "/nodes", ""); code != http.StatusOK {
		t.Fatal(code)
	}

	// Replaying the recorded sign-in: the challenge was single-use.
	login := f.evil.last("/login")
	if code, out := f.evil.replay(t, login); code != http.StatusUnauthorized {
		t.Fatalf("replayed sign-in: %d %s", code, out)
	}

	// A change, then the relay replays it: refused, not repeated.
	avail := "/nodes/" + string(f.nodeID) + "/availability"
	if code, out := mustRemote(t, c, "PUT", avail, `{"mode":"paused"}`); code != http.StatusOK {
		t.Fatalf("remote availability change: %d %s", code, out)
	}
	changed := f.auditCount(t, "security", "node.availability-changed", "remote")
	if changed != 1 {
		t.Fatalf("availability changes audited: %d", changed)
	}
	recorded := f.evil.last("/call")
	if code, out := f.evil.replay(t, recorded); code != http.StatusConflict {
		t.Fatalf("replayed call: %d %s", code, out)
	}
	ruleIs := func(want string) {
		t.Helper()
		_, body := f.operatorDo(t, "GET", "/nodes/"+string(f.nodeID), "")
		if !strings.Contains(string(body), `"mode":"`+want+`"`) {
			t.Fatalf("availability rule is not %s: %s", want, body)
		}
	}
	ruleIs("paused")

	// Changing a request in flight: its body, its sequence number, or the
	// session it claims to belong to.
	other := f.client(v)
	if code, _ := mustRemote(t, other, "GET", "/nodes", ""); code != http.StatusOK {
		t.Fatal(code)
	}
	otherSession := f.evil.last("/call").header.Get(remoteaccess.HeaderSession)
	for _, action := range []string{"flip-request", "seq", "session:" + otherSession, "drop-body"} {
		f.evil.arm(action)
		if _, _, err := remoteCall(c, "PUT", avail, `{"mode":"always"}`); err == nil {
			t.Fatalf("%s: the changed request was accepted", action)
		}
	}
	ruleIs("paused")
	if n := f.auditCount(t, "security", "node.availability-changed", "remote"); n != changed {
		t.Fatalf("a refused request changed something: %d audited changes", n)
	}
	if n := f.auditCount(t, "noise", "remote.call-refused", ""); n == 0 {
		t.Fatal("refused calls are not in the noise log")
	}

	// Changing, cutting or forging the reply: the client never accepts it.
	for _, action := range []string{"flip-head", "flip-body", "truncate", "forge-sealed", "forge-plain"} {
		f.evil.arm(action)
		code, out, err := remoteCall(c, "GET", "/nodes", "")
		if err == nil {
			t.Fatalf("%s: the client accepted a changed reply: %d %s", action, code, out)
		}
	}
	if code, _ := mustRemote(t, c, "GET", "/nodes", ""); code != http.StatusOK {
		t.Fatal("the session did not survive the relay's attempts")
	}

	// And the relay can't sign in itself without the key.
	guess := &remoteaccess.Client{BaseURL: c.BaseURL, Key: strings.Repeat("Z", 24)}
	if _, _, err := remoteCall(guess, "GET", "/nodes", ""); err == nil || !strings.Contains(err.Error(), "not right") {
		t.Fatalf("a guessed key: %v", err)
	}
}

// evilRelay stands where the relay's gateway stands: it sees every byte
// between the remote browser and the manager, and can change any of it.
type evilRelay struct {
	target string
	client *http.Transport

	mu       sync.Mutex
	requests []recordedRequest
	seen     bytes.Buffer
	next     string // one action for the next /call
}

type recordedRequest struct {
	method, path string
	header       http.Header
	body         []byte
}

func (e *evilRelay) arm(action string) {
	e.mu.Lock()
	e.next = action
	e.mu.Unlock()
}

func (e *evilRelay) seenBytes() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return bytes.Clone(e.seen.Bytes())
}

func (e *evilRelay) last(suffix string) recordedRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.requests) - 1; i >= 0; i-- {
		if strings.HasSuffix(e.requests[i].path, suffix) {
			return e.requests[i]
		}
	}
	return recordedRequest{}
}

func (e *evilRelay) forward(method, path string, header http.Header, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, e.target+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header
	return e.client.RoundTrip(req)
}

// replay sends a recorded request again, byte for byte.
func (e *evilRelay) replay(t *testing.T, r recordedRequest) (int, string) {
	t.Helper()
	if r.path == "" {
		t.Fatal("nothing recorded to replay")
	}
	resp, err := e.forward(r.method, r.path, r.header.Clone(), r.body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *evilRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	header := r.Header.Clone()
	e.mu.Lock()
	e.requests = append(e.requests, recordedRequest{method: r.Method, path: r.URL.Path, header: header.Clone(), body: bytes.Clone(body)})
	e.seen.WriteString(r.Method + " " + r.URL.Path + "\n")
	for k, vs := range header {
		e.seen.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
	}
	e.seen.Write(body)
	action := ""
	if strings.HasSuffix(r.URL.Path, "/call") {
		action, e.next = e.next, ""
	}
	e.mu.Unlock()

	switch {
	case action == "flip-request":
		body[len(body)/2] ^= 0x01
	case action == "drop-body":
		body = body[:len(body)-1]
	case action == "seq":
		seq, _ := strconv.ParseUint(header.Get(remoteaccess.HeaderSeq), 10, 64)
		header.Set(remoteaccess.HeaderSeq, strconv.FormatUint(seq+1, 10))
	case strings.HasPrefix(action, "session:"):
		header.Set(remoteaccess.HeaderSession, strings.TrimPrefix(action, "session:"))
	case action == "forge-sealed":
		w.Header().Set(remoteaccess.HeaderSealed, "1")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte{0, 0, 0, 40})
		w.Write(bytes.Repeat([]byte{0x42}, 40))
		return
	case action == "forge-plain":
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"nodeId":"forged-by-the-relay"}]`))
		return
	}
	resp, err := e.forward(r.Method, r.URL.Path, header, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// The page itself is public code (it names every route); what the
	// relay must not learn is in the API traffic, all of it POSTs.
	api := r.Method == http.MethodPost
	if action != "" {
		reply, _ := io.ReadAll(resp.Body)
		e.record(reply)
		switch action {
		case "flip-head":
			reply[6] ^= 0x01 // inside the first (head) frame
		case "flip-body":
			reply[len(reply)-30] ^= 0x01 // inside a later frame
		case "truncate":
			reply = reply[:len(reply)-20] // the end frame is lost
		}
		w.Write(reply)
		return
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if api {
				e.record(buf[:n])
			}
			w.Write(buf[:n])
			rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (e *evilRelay) record(b []byte) {
	e.mu.Lock()
	e.seen.Write(b)
	e.mu.Unlock()
}
