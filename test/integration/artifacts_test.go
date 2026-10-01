package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/multi"
	relaytransport "home-harness/internal/transport/relay"
	"home-harness/internal/transport/ws"
)

func hexSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func openArtifactStore(t *testing.T, maxBytes int64) *artifacts.Store {
	t.Helper()
	store, err := artifacts.Open(artifacts.Config{Dir: filepath.Join(t.TempDir(), "artifacts"), MaxBytes: maxBytes, TotalBytes: 64 << 20})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	return store
}

type artifactManager struct {
	srv         *manager.Server
	api         string
	fingerprint string
}

// startArtifactManager wires the workload-file routes onto the agent
// transport exactly as cmd/manager does. With useTLS the agents' file
// transfers take the production path: HTTPS pinned to the manager cert.
func startArtifactManager(t *testing.T, addr string, useTLS bool) artifactManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	var fp string
	if useTLS {
		cert, err := mtls.LoadOrCreateCert(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		transport, fp = ws.NewTLSServer(cert), mtls.Fingerprint(cert)
	}
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second,
		ReconcileInterval: 100 * time.Millisecond, Artifacts: openArtifactStore(t, 1<<20),
	})
	transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	go srv.Run(ctx)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	return artifactManager{srv: srv, api: api.URL, fingerprint: fp}
}

func startFileAgent(t *testing.T, ctx context.Context, m artifactManager, addr, name string) *agent.Agent {
	t.Helper()
	cfg := agent.Config{
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond, HostFingerprint: "-",
	}
	var transport domain.Transport = ws.New()
	if m.fingerprint != "" {
		transport = ws.NewTLSClient(mtls.PinnedClientConfig(m.fingerprint))
		cfg.ManagerFingerprint = m.fingerprint
	} else {
		cfg.Insecure = true // plaintext test manager: plain-HTTP transfers (workloads stay enabled)
	}
	a, err := agent.New(transport, cfg)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a
}

func uploadArtifact(t *testing.T, api string, content []byte) string {
	t.Helper()
	resp, err := http.Post(api+"/artifacts", "application/octet-stream", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /artifacts: %s %s", resp.Status, b)
	}
	var info struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	json.NewDecoder(resp.Body).Decode(&info)
	if info.SHA256 != hexSHA(content) || info.Size != int64(len(content)) {
		t.Fatalf("stored %+v", info)
	}
	return info.SHA256
}

func postWorkload(t *testing.T, api string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(api+"/workloads", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"raw": string(raw)}
	}
	return resp.StatusCode, out
}

type fileWorkloadView struct {
	State       domain.WorkloadState `json:"state"`
	Error       string               `json:"error"`
	Stderr      string               `json:"stderr"`
	RestartCnt  int                  `json:"restartCount"`
	OutputFiles []domain.ArtifactRef `json:"outputFiles"`
}

func getFileWorkload(t *testing.T, api, id string) fileWorkloadView {
	t.Helper()
	var v fileWorkloadView
	getJSON(t, api+"/workloads/"+id, &v)
	return v
}

// sortCommand sorts in/data.txt into out/sorted.txt in the work dir.
func sortCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		// Not sort.exe: a Git Bash PATH puts GNU sort first.
		return "powershell", []string{"-NoProfile", "-Command", `Get-Content in\data.txt | Sort-Object | Set-Content out\sorted.txt`}
	}
	return "sort", []string{"in/data.txt", "-o", "out/sorted.txt"}
}

func runSortWorkload(t *testing.T, m artifactManager, target domain.NodeID, extra map[string]any) string {
	t.Helper()
	sha := uploadArtifact(t, m.api, []byte("banana\ncherry\napple\n"))
	cmd, args := sortCommand()
	body := map[string]any{
		"target": target, "command": cmd, "args": args,
		"inputs":  []map[string]string{{"name": "in/data.txt", "sha256": sha}},
		"outputs": []string{"out/sorted.txt"},
	}
	for k, v := range extra {
		body[k] = v
	}
	code, out := postWorkload(t, m.api, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST /workloads: %d %v", code, out)
	}
	return out["id"].(string)
}

func expectSortedOutput(t *testing.T, m artifactManager, v fileWorkloadView) {
	t.Helper()
	if len(v.OutputFiles) != 1 || v.OutputFiles[0].Name != "out/sorted.txt" {
		t.Fatalf("outputFiles = %+v", v.OutputFiles)
	}
	resp, err := http.Get(m.api + "/artifacts/" + v.OutputFiles[0].SHA256 + "?name=out/sorted.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.ReplaceAll(string(body), "\r", ""); got != "apple\nbanana\ncherry\n" {
		t.Fatalf("downloaded output %q", got)
	}
	// Agent-produced bytes on the credential-holding origin: always an
	// opaque attachment, never something a browser renders.
	h := resp.Header
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Content-Security-Policy") != "sandbox" || h.Get("Content-Disposition") != `attachment; filename="sorted.txt"` {
		t.Fatalf("unsafe download headers: %v", h)
	}
}

// End to end on the production path (TLS, pinned): an uploaded file is
// fetched into the workload's working directory, the program transforms
// it, and the declared output comes back hash-verified and downloadable.
func TestWorkloadFilesRoundTripOverTLS(t *testing.T) {
	const addr = "127.0.0.1:19530"
	m := startArtifactManager(t, addr, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := startFileAgent(t, ctx, m, addr, "tls-file-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	id := runSortWorkload(t, m, "", nil)
	var v fileWorkloadView
	waitFor(t, 20*time.Second, func() bool {
		v = getFileWorkload(t, m.api, id)
		return v.State == domain.WorkloadCompleted || v.State == domain.WorkloadFailed
	})
	if v.State != domain.WorkloadCompleted {
		t.Fatalf("got %s: %s (stderr %q)", v.State, v.Error, v.Stderr)
	}
	expectSortedOutput(t, m, v)
}

// The relay is a raw byte splice, so uploads (request bodies) must pass
// through it as well as downloads.
func TestWorkloadFilesThroughRelay(t *testing.T) {
	const lanAddr = "127.0.0.1:19531"
	relayAddr := startTestRelayServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lan, rt := ws.New(), relaytransport.New(relaySessionToken)
	srv := manager.NewServer(multiTransport(lan, lanAddr, rt, relayAddr), nil, manager.Config{
		Addr: lanAddr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second, Artifacts: openArtifactStore(t, 1<<20),
	})
	lan.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	rt.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	go srv.Run(ctx)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()
	m := artifactManager{srv: srv, api: api.URL}

	client := relaytransport.NewClient(relaySessionToken)
	a, err := agent.New(client, agent.Config{
		ManagerAddr: relayAddr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), "relay-file-agent"),
		WorkDir: filepath.Join(t.TempDir(), "relay-work"), Name: "relay-file-agent", HeartbeatInterval: 100 * time.Millisecond,
		HostFingerprint: "-", Insecure: true,
		SelfUpdateHTTPClient: client.HTTPClient(relayAddr), SelfUpdateBaseURL: "http://manager",
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	waitFor(t, 5*time.Second, func() bool { rec, ok := srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	id := runSortWorkload(t, m, a.NodeID(), nil)
	var v fileWorkloadView
	waitFor(t, 20*time.Second, func() bool {
		v = getFileWorkload(t, m.api, id)
		return v.State == domain.WorkloadCompleted || v.State == domain.WorkloadFailed
	})
	if v.State != domain.WorkloadCompleted {
		t.Fatalf("over the relay: got %s: %s", v.State, v.Error)
	}
	expectSortedOutput(t, m, v)
}

// A restart is a new assignment: it gets a fresh token and fetches its
// inputs again into a fresh working directory.
func TestRestartedFileWorkloadRefetchesInputs(t *testing.T) {
	const addr = "127.0.0.1:19532"
	m := startArtifactManager(t, addr, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := startFileAgent(t, ctx, m, addr, "restart-file-agent")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	id := runSortWorkload(t, m, "", map[string]any{"restartPolicy": "always"})
	var v fileWorkloadView
	waitFor(t, 30*time.Second, func() bool {
		v = getFileWorkload(t, m.api, id)
		return v.RestartCnt >= 1 && v.State == domain.WorkloadCompleted
	})
	expectSortedOutput(t, m, v)
	m.srv.CancelWorkload(ctx, domain.WorkloadID(id))
}

// A raw enrolled agent holding one assignment's token: everything outside
// that assignment's declared files is refused, each output is accepted
// once, and only outputs the manager really received are ever recorded.
func TestTransferTokenIsScopedToItsAssignment(t *testing.T) {
	const addr = "127.0.0.1:19533"
	m := startArtifactManager(t, addr, false)
	nodeID, closer := registerRawNodeWithManifest(t, addr, "token-agent", func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
		mf.AgentFeatures = []string{domain.FeatureArtifacts}
		mf.WorkloadSlots = 4
	})
	defer closer.Close()
	conn := closer.(domain.Conn)
	waitFor(t, 3*time.Second, func() bool { rec, ok := m.srv.Registry.Get(nodeID); return ok && rec.State == domain.NodeReady })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	assigns := make(chan protocol.WorkloadAssignPayload, 8)
	go func() {
		for {
			data, err := conn.Receive(ctx)
			if err != nil {
				return
			}
			if env, err := protocol.Decode(data); err == nil && env.Type == protocol.MsgWorkloadAssign {
				var p protocol.WorkloadAssignPayload
				if env.DecodePayload(&p) == nil {
					assigns <- p
				}
			}
		}
	}()
	next := func() protocol.WorkloadAssignPayload {
		t.Helper()
		select {
		case p := <-assigns:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no ASSIGN")
			return protocol.WorkloadAssignPayload{}
		}
	}
	inA, inB := []byte("input A"), []byte("input B")
	shaA, shaB := uploadArtifact(t, m.api, inA), uploadArtifact(t, m.api, inB)
	submit := func(sha string, outputs []string) protocol.WorkloadAssignPayload {
		t.Helper()
		code, out := postWorkload(t, m.api, map[string]any{
			"target": nodeID, "command": "x", "inputs": []map[string]string{{"name": "in.bin", "sha256": sha}}, "outputs": outputs,
		})
		if code != http.StatusAccepted {
			t.Fatalf("submit: %d %v", code, out)
		}
		return next()
	}
	pa := submit(shaA, []string{"out.txt", "second.txt"})
	pb := submit(shaB, []string{"out.txt"})
	if pa.ArtifactToken == "" || pb.ArtifactToken == "" || pa.ArtifactToken == pb.ArtifactToken {
		t.Fatal("each assignment needs its own token")
	}
	// The token must not leak through the operator API.
	raw, _ := http.Get(m.api + "/workloads/" + string(pa.Workload.ID))
	rb, _ := io.ReadAll(raw.Body)
	raw.Body.Close()
	if strings.Contains(string(rb), pa.ArtifactToken) {
		t.Fatal("GET /workloads/{id} exposes the transfer token")
	}

	base := "http://" + addr + "/workload-artifacts/"
	do := func(method, url, token string, body []byte, sha string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if sha != "" {
			req.Header.Set("X-Artifact-SHA256", sha)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	idA := string(pa.Workload.ID)
	inputURL := base + idA + "/inputs/" + shaA
	for _, c := range []struct {
		name, url, token string
		want             int
	}{
		{"no token", inputURL, "", 401},
		{"made-up token", inputURL, strings.Repeat("ab", 32), 401},
		{"B's token on A's path", inputURL, pb.ArtifactToken, 403},
		{"B's token for its own input on A's path", base + idA + "/inputs/" + shaB, pb.ArtifactToken, 403},
		{"A's token for B's input", base + idA + "/inputs/" + shaB, pa.ArtifactToken, 403},
		{"bad sha", base + idA + "/inputs/..%2f..%2fx", pa.ArtifactToken, 400},
	} {
		if code, _ := do("GET", c.url, c.token, nil, ""); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
	}
	if code, body := do("GET", inputURL, pa.ArtifactToken, nil, ""); code != 200 || string(body) != string(inA) {
		t.Fatalf("own input: %d %q", code, body)
	}

	out := []byte("result of A")
	outURL := base + idA + "/outputs/out.txt"
	if code, _ := do("PUT", base+idA+"/outputs/undeclared.txt", pa.ArtifactToken, out, hexSHA(out)); code != 403 {
		t.Errorf("undeclared output: got %d, want 403", code)
	}
	if code, _ := do("PUT", outURL, pa.ArtifactToken, out, ""); code != 400 {
		t.Errorf("missing hash header: got %d, want 400", code)
	}
	if code, _ := do("PUT", outURL, pa.ArtifactToken, out, hexSHA([]byte("other"))); code != 400 {
		t.Errorf("hash mismatch: got %d, want 400", code)
	}
	if code, _ := do("PUT", outURL, pa.ArtifactToken, out, hexSHA(out)); code != 201 {
		t.Fatalf("good upload: got %d", code)
	}
	if code, _ := do("PUT", outURL, pa.ArtifactToken, []byte("again"), hexSHA([]byte("again"))); code != 409 {
		t.Errorf("second upload of one output: got %d, want 409", code)
	}

	// COMPLETED claiming out.txt (real) plus second.txt (never uploaded),
	// and a forged hash for nothing: only the real one is recorded, and a
	// declared output the manager never got makes it FAILED.
	report := func(st domain.WorkloadStatus) {
		env, _ := protocol.NewEnvelope(protocol.MsgWorkloadStatus, nodeID, domain.ManagerNodeID, protocol.WorkloadStatusPayload{Status: st})
		wire, _ := protocol.Encode(env)
		if err := conn.Send(ctx, wire); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	report(domain.WorkloadStatus{ID: pa.Workload.ID, Target: nodeID, State: domain.WorkloadRunning, StartedAt: now})
	report(domain.WorkloadStatus{ID: pa.Workload.ID, Target: nodeID, State: domain.WorkloadCompleted, StartedAt: now, FinishedAt: now, Outputs: []domain.ArtifactRef{
		{Name: "out.txt", SHA256: hexSHA(out), Size: int64(len(out))},
		{Name: "second.txt", SHA256: hexSHA([]byte("forged")), Size: 6},
	}})
	var v fileWorkloadView
	waitFor(t, 3*time.Second, func() bool {
		v = getFileWorkload(t, m.api, idA)
		return v.State != domain.WorkloadPending && v.State != domain.WorkloadRunning
	})
	if v.State != domain.WorkloadFailed || !strings.Contains(v.Error, "second.txt") {
		t.Fatalf("got %s (%s), want FAILED naming the undelivered output", v.State, v.Error)
	}
	if len(v.OutputFiles) != 1 || v.OutputFiles[0].SHA256 != hexSHA(out) {
		t.Fatalf("recorded outputs %+v, want only the uploaded one", v.OutputFiles)
	}
	// The finished assignment's token is dead.
	if code, _ := do("GET", inputURL, pa.ArtifactToken, nil, ""); code != 401 {
		t.Fatalf("token after the workload finished: got %d, want 401", code)
	}
}

// Old agents (no artifacts.v1 feature) would ignore the files and run
// the command anyway, so placement never picks them.
func TestFileWorkloadNeverPlacedOnAgentWithoutTheFeature(t *testing.T) {
	const addr = "127.0.0.1:19534"
	m := startArtifactManager(t, addr, false)
	nodeID, closer := registerRawNodeWithManifest(t, addr, "old-agent", func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
	})
	defer closer.Close()
	waitFor(t, 3*time.Second, func() bool { rec, ok := m.srv.Registry.Get(nodeID); return ok && rec.State == domain.NodeReady })
	sha := uploadArtifact(t, m.api, []byte("x"))
	for _, target := range []string{"", string(nodeID)} {
		code, out := postWorkload(t, m.api, map[string]any{"target": target, "command": "x", "inputs": []map[string]string{{"name": "a", "sha256": sha}}})
		if code != http.StatusConflict {
			t.Fatalf("target=%q: got %d %v, want 409", target, code, out)
		}
	}
	// The same command without files is fine on it.
	if code, out := postWorkload(t, m.api, map[string]any{"target": nodeID, "command": "x"}); code != http.StatusAccepted {
		t.Fatalf("plain workload: %d %v", code, out)
	}
}

func TestSubmissionsWithBadFilesAreRejected(t *testing.T) {
	const addr = "127.0.0.1:19535"
	m := startArtifactManager(t, addr, false)
	sha := uploadArtifact(t, m.api, []byte("x"))
	in := func(name, sha string) []map[string]string { return []map[string]string{{"name": name, "sha256": sha}} }
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"unknown input", map[string]any{"command": "x", "inputs": in("a", strings.Repeat("0", 64))}},
		{"traversal", map[string]any{"command": "x", "inputs": in("../a", sha)}},
		{"case collision", map[string]any{"command": "x", "inputs": in("Data", sha), "outputs": []string{"data"}}},
		{"prefix collision", map[string]any{"command": "x", "inputs": in("a", sha), "outputs": []string{"a/b"}}},
		{"files on filesystem.read", map[string]any{"capability": "filesystem.read", "params": map[string]string{"path": "x"}, "outputs": []string{"o"}}},
	} {
		if code, out := postWorkload(t, m.api, c.body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d %v, want 400", c.name, code, out)
		}
	}

	// No store at all: files are refused, plain workloads unaffected.
	plain := startManager(t, "127.0.0.1:19536", 2*time.Second)
	api := httptest.NewServer(plain.NewHTTPHandler())
	defer api.Close()
	if code, _ := postWorkload(t, api.URL, map[string]any{"command": "x", "outputs": []string{"o"}}); code != http.StatusBadRequest {
		t.Fatalf("files without a store: got %d, want 400", code)
	}
	resp, _ := http.Post(api.URL+"/artifacts", "application/octet-stream", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST /artifacts without a store: got %d", resp.StatusCode)
	}
}

func TestArtifactAPI(t *testing.T) {
	const addr = "127.0.0.1:19537"
	m := startArtifactManager(t, addr, false)
	post := func(body []byte, sha string) int {
		req, _ := http.NewRequest("POST", m.api+"/artifacts", bytes.NewReader(body))
		if sha != "" {
			req.Header.Set("X-Artifact-SHA256", sha)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post([]byte("abc"), hexSHA([]byte("abd"))); code != 400 {
		t.Errorf("wrong expected hash: %d", code)
	}
	if code := post(make([]byte, (1<<20)+1), ""); code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the per-file limit: %d", code)
	}
	if code := post([]byte("abc"), hexSHA([]byte("abc"))); code != 201 {
		t.Errorf("good upload: %d", code)
	}
	var list struct {
		Artifacts []artifacts.Info `json:"artifacts"`
		Used      int64            `json:"usedBytes"`
	}
	getJSON(t, m.api+"/artifacts", &list)
	if len(list.Artifacts) != 1 || list.Used != 3 {
		t.Fatalf("list = %+v", list)
	}

	// A queued workload's input can't be deleted; once it's gone, it can.
	keep := uploadArtifact(t, m.api, []byte("needed by a queued workload"))
	code, out := postWorkload(t, m.api, map[string]any{"command": "x", "inputs": []map[string]string{{"name": "a", "sha256": keep}}})
	if code != http.StatusConflict { // no node at all: rejected, so nothing references it
		t.Fatalf("submit with no nodes: %d %v", code, out)
	}
	del := func(sha string) int {
		req, _ := http.NewRequest("DELETE", m.api+"/artifacts/"+sha, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	nodeID, closer := registerRawNodeWithManifest(t, addr, "busy-agent", func(mf *domain.Manifest) {
		mf.Capabilities = []domain.Capability{{Name: domain.CapabilitySystemExecute}}
		mf.AgentFeatures = []string{domain.FeatureArtifacts}
	})
	defer closer.Close()
	waitFor(t, 3*time.Second, func() bool { rec, ok := m.srv.Registry.Get(nodeID); return ok && rec.State == domain.NodeReady })
	for i := 0; i < 2; i++ { // the second one queues behind the first
		if code, out := postWorkload(t, m.api, map[string]any{"target": nodeID, "command": "x", "inputs": []map[string]string{{"name": "a", "sha256": keep}}}); code != http.StatusAccepted {
			t.Fatalf("submit %d: %d %v", i, code, out)
		}
	}
	if code := del(keep); code != http.StatusConflict {
		t.Fatalf("deleting a live input: got %d, want 409", code)
	}
	if code := del(hexSHA([]byte("abc"))); code != http.StatusNoContent {
		t.Fatalf("deleting an unused artifact: got %d", code)
	}
	resp, _ := http.Get(m.api + "/artifacts/" + hexSHA([]byte("abc")))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted artifact: got %d, want 404", resp.StatusCode)
	}
	resp, _ = http.Get(m.api + "/artifacts/not-a-hash")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid id: got %d, want 400", resp.StatusCode)
	}
}

func multiTransport(lan domain.Transport, lanAddr string, relay domain.Transport, relayAddr string) domain.Transport {
	return multi.New().Add(lan, lanAddr).Add(relay, relayAddr)
}
