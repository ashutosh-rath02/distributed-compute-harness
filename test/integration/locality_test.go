package integration

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

// Data-local placement (manager/locality.go, agent/inputcache.go).

// localityManager is startArtifactManager that also counts the input
// downloads agents make, so a test can tell a cached input from a
// downloaded one.
type localityManager struct {
	artifactManager
	inputGets *atomic.Int64
}

func startLocalityManager(t *testing.T, addr string) localityManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 30 * time.Second,
		ReconcileInterval: 100 * time.Millisecond, Artifacts: openArtifactStore(t, 1<<20),
	})
	gets := &atomic.Int64{}
	files := srv.ArtifactTransferHandler()
	transport.Handle("/workload-artifacts/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/inputs/") {
			gets.Add(1)
		}
		files.ServeHTTP(w, r)
	}))
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	return localityManager{artifactManager{srv: srv, api: api.URL}, gets}
}

// startCachingAgent is startFileAgentWithSlots with the input cache on.
func startCachingAgent(t *testing.T, ctx context.Context, addr, name string, slots int) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn, WorkloadSlots: slots, OllamaURL: "127.0.0.1:1",
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond,
		HostFingerprint: "-", Insecure: true, InputCacheBytes: 8 << 20,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func waitReady(t *testing.T, srv *manager.Server, ids ...domain.NodeID) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		for _, id := range ids {
			if rec, ok := srv.Registry.Get(id); !ok || rec.State != domain.NodeReady {
				return false
			}
		}
		return true
	})
}

// hashFile submits file.hash over the stored file sha (pinned to target,
// or placed when target is empty) and returns the workload ID and where
// it went.
func hashFile(t *testing.T, api string, target domain.NodeID, sha string) (string, domain.NodeID) {
	t.Helper()
	code, out := postWorkload(t, api, map[string]any{
		"target": target, "capability": "file.hash",
		"inputs": []map[string]string{{"name": "big.bin", "sha256": sha}},
	})
	if code != http.StatusAccepted {
		t.Fatalf("POST /workloads: %d %v", code, out)
	}
	return out["id"].(string), domain.NodeID(out["target"].(string))
}

func waitCompleted(t *testing.T, api, id string) {
	t.Helper()
	var v fileWorkloadView
	waitFor(t, 20*time.Second, func() bool {
		v = getFileWorkload(t, api, id)
		return v.State == domain.WorkloadCompleted || v.State == domain.WorkloadFailed || v.State == domain.WorkloadCanceled
	})
	if v.State != domain.WorkloadCompleted {
		t.Fatalf("workload %s: %s %s", id, v.State, v.Error)
	}
}

// A second job (or a re-run) on a big file goes to the idle device that
// already has it, and that device copies it from its cache instead of
// downloading it — in both directions, so neither node ID nor free memory
// can be what decided.
func TestSecondRunOnTheSameFileGoesToTheDeviceThatHasIt(t *testing.T) {
	const addr = "127.0.0.1:19650"
	m := startLocalityManager(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startCachingAgent(t, ctx, addr, "loc-a", 2)
	b := startCachingAgent(t, ctx, addr, "loc-b", 2)
	waitReady(t, m.srv, a.NodeID(), b.NodeID())

	f := uploadArtifact(t, m.api, randomBytes(t, 600<<10))
	g := uploadArtifact(t, m.api, randomBytes(t, 600<<10))

	for _, c := range []struct {
		sha    string
		holder domain.NodeID
	}{{f, a.NodeID()}, {g, b.NodeID()}} {
		id, _ := hashFile(t, m.api, c.holder, c.sha) // the first run, pinned
		waitCompleted(t, m.api, id)
		waitFor(t, 5*time.Second, func() bool { return m.srv.Registry.HoldsInput(c.holder, c.sha) })
	}

	// F again, as a second job: its task goes to a, without a download.
	before := m.inputGets.Load()
	code, job, raw := postJob(t, m.api, map[string]any{"tasks": []map[string]any{{
		"capability": "file.hash", "inputs": []map[string]string{{"name": "big.bin", "sha256": f}},
	}}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	done := waitJob(t, m.api, job.ID, 20*time.Second)
	if done.State != domain.JobCompleted || done.Tasks[0].Node != a.NodeID() {
		t.Fatalf("second job on F: %s on %s, want COMPLETED on %s (which has F)", done.State, done.Tasks[0].Node, a.NodeID())
	}
	// G again, as a plain re-run: to b.
	id, target := hashFile(t, m.api, "", g)
	if target != b.NodeID() {
		t.Fatalf("re-run on G went to %s, want %s (which has G)", target, b.NodeID())
	}
	waitCompleted(t, m.api, id)
	if got := m.inputGets.Load() - before; got != 0 {
		t.Fatalf("%d input download(s) for files the devices already had", got)
	}
}

// Spreading stays first: a job's tasks on one cached file still go to
// every device with a free slot, the file's device taking only the first.
func TestJobOnACachedFileStillSpreadsOverIdleDevices(t *testing.T) {
	const addr = "127.0.0.1:19651"
	m := startLocalityManager(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a := startCachingAgent(t, ctx, addr, "spread-a", 2)
	b := startCachingAgent(t, ctx, addr, "spread-b", 2)
	waitReady(t, m.srv, a.NodeID(), b.NodeID())

	f := uploadArtifact(t, m.api, randomBytes(t, 300<<10))
	id, _ := hashFile(t, m.api, b.NodeID(), f)
	waitCompleted(t, m.api, id)
	waitFor(t, 5*time.Second, func() bool { return m.srv.Registry.HoldsInput(b.NodeID(), f) })

	cmd, args := sleepArgs("2") // both tasks in flight at once
	task := map[string]any{"command": cmd, "args": args, "inputs": []map[string]string{{"name": "big.bin", "sha256": f}}}
	code, job, raw := postJob(t, m.api, map[string]any{"tasks": []map[string]any{task, task}})
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", code, raw)
	}
	done := waitJob(t, m.api, job.ID, 30*time.Second)
	if done.State != domain.JobCompleted {
		t.Fatalf("job: %s %s", done.State, done.Error)
	}
	if done.Tasks[0].Node == done.Tasks[1].Node {
		t.Fatalf("both tasks ran on %s: locality must not outrank spreading", done.Tasks[0].Node)
	}
}

// registerCacheNode registers a raw node that can take file workloads,
// with the given agent features and cache report.
func registerCacheNode(t *testing.T, addr, dir string, features []string, cache *domain.InputCacheReport) (domain.NodeID, domain.Conn) {
	t.Helper()
	id := identityFor(t, dir)
	manifest := domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Node: domain.Node{Identity: id.Identity, Name: filepath.Base(dir)},
		Capabilities: []domain.Capability{{Name: domain.CapabilitySystemExecute}}, AgentFeatures: features, WorkloadSlots: 4}
	conn := dialWithRetry(t, addr)
	env, _ := protocol.NewEnvelope(protocol.MsgRegister, id.NodeID, domain.ManagerNodeID, protocol.RegisterPayload{
		Manifest: manifest, PairingToken: pairingToken, Signature: id.Sign(protocol.RegisterSignedData(pairingToken, id.NodeID)), Cache: cache,
	})
	wire, _ := protocol.Encode(env)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatalf("send REGISTER: %v", err)
	}
	if _, err := conn.Receive(ctx); err != nil {
		t.Fatalf("receive REGISTER reply: %v", err)
	}
	return id.NodeID, conn
}

func sendRaw(t *testing.T, conn domain.Conn, from domain.NodeID, typ protocol.MessageType, payload any) {
	t.Helper()
	env, _ := protocol.NewEnvelope(typ, from, domain.ManagerNodeID, payload)
	wire, _ := protocol.Encode(env)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
}

// What a device says it holds counts only from agents advertising the
// input cache: an older agent (or anything claiming without the feature)
// gets no preference. Raw nodes make the claims exact; neither ever
// heartbeats memory, so only node ID or locality can decide.
func TestCacheHintsCountOnlyFromAgentsWithTheFeature(t *testing.T) {
	const addr = "127.0.0.1:19652"
	m := startLocalityManager(t, addr)
	f := uploadArtifact(t, m.api, randomBytes(t, 64<<10))
	claim := &domain.InputCacheReport{Prefixes: []string{f[:domain.InputCachePrefixLen]}}

	// Two identities; "late" is the one that loses ties on node ID.
	d1, d2 := filepath.Join(t.TempDir(), "hint-1"), filepath.Join(t.TempDir(), "hint-2")
	early, late := d1, d2
	if identityFor(t, d1).NodeID > identityFor(t, d2).NodeID {
		early, late = d2, d1
	}
	earlyID, earlyConn := registerCacheNode(t, addr, early, []string{domain.FeatureArtifacts, domain.FeatureInputCache}, &domain.InputCacheReport{})
	defer earlyConn.Close()
	lateID, lateConn := registerCacheNode(t, addr, late, []string{domain.FeatureArtifacts}, claim) // claims F, no feature
	waitReady(t, m.srv, earlyID, lateID)

	cmd, args := sleepArgs("1")
	place := func() (domain.NodeID, string) {
		t.Helper()
		code, out := postWorkload(t, m.api, map[string]any{"command": cmd, "args": args, "inputs": []map[string]string{{"name": "f.bin", "sha256": f}}})
		if code != http.StatusAccepted {
			t.Fatalf("POST /workloads: %d %v", code, out)
		}
		return domain.NodeID(out["target"].(string)), out["id"].(string)
	}
	finish := func(node domain.NodeID, conn domain.Conn, id string) {
		t.Helper()
		now := time.Now().UTC()
		sendRaw(t, conn, node, protocol.MsgWorkloadStatus, protocol.WorkloadStatusPayload{Status: domain.WorkloadStatus{
			ID: domain.WorkloadID(id), Target: node, State: domain.WorkloadCompleted, StartedAt: now, FinishedAt: now}})
		waitFor(t, 3*time.Second, func() bool { return workloadState(m.srv, domain.WorkloadID(id)) == domain.WorkloadCompleted })
	}

	if got, id := place(); got != earlyID {
		t.Fatalf("a claim without the feature won placement (%s, want %s)", got, earlyID)
	} else {
		finish(earlyID, earlyConn, id)
	}

	// The same device updated (it reconnects advertising the cache) and
	// reporting F in a heartbeat: now it is preferred.
	lateConn.Close()
	lateID, lateConn = registerCacheNode(t, addr, late, []string{domain.FeatureArtifacts, domain.FeatureInputCache}, &domain.InputCacheReport{})
	defer lateConn.Close()
	waitReady(t, m.srv, lateID)
	sendRaw(t, lateConn, lateID, protocol.MsgHeartbeat, protocol.HeartbeatPayload{
		RuntimeState: domain.RuntimeState{NodeID: lateID, State: domain.NodeReady, LastHeartbeat: time.Now().UTC()}, Cache: claim})
	waitFor(t, 3*time.Second, func() bool { return m.srv.Registry.HoldsInput(lateID, f) })
	got, id := place()
	if got != lateID {
		t.Fatalf("the device holding F (with the feature) wasn't preferred: %s, want %s", got, lateID)
	}
	finish(lateID, lateConn, id)
}
