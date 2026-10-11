package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// Chaos tests: break the system on purpose and assert what must still be
// true afterwards (nothing lost, nothing run twice, recovery within a
// bound), not merely that nothing crashed. Ports 19720-19729.

// ---- fault injection: a TCP proxy between agents and the manager

type chaosProxy struct {
	ln     net.Listener
	target string

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	cutAfter int64         // cut a stream once this many bytes went manager->agent...
	cutsLeft int           // ...this many times
	delay    time.Duration // added to every chunk, both directions
	cuts     atomic.Int32
}

func newChaosProxy(t *testing.T, listen, target string) *chaosProxy {
	t.Helper()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	p := &chaosProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	t.Cleanup(func() { ln.Close(); p.DropAll() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *chaosProxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
	p.mu.Unlock()
}

// DropAll abruptly closes every connection through the proxy.
func (p *chaosProxy) DropAll() {
	p.mu.Lock()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
}

func (p *chaosProxy) setDelay(d time.Duration) { p.mu.Lock(); p.delay = d; p.mu.Unlock() }

func (p *chaosProxy) serve(client net.Conn) {
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.track(client, true)
	p.track(server, true)
	closeBoth := func() { client.Close(); server.Close(); p.track(client, false); p.track(server, false) }
	pipe := func(dst, src net.Conn, serverToClient bool) {
		defer closeBoth()
		buf := make([]byte, 16<<10)
		var sent int64
		for {
			n, err := src.Read(buf)
			if n > 0 {
				p.mu.Lock()
				d := p.delay
				cut := serverToClient && p.cutsLeft > 0 && sent+int64(n) > p.cutAfter
				if cut {
					p.cutsLeft--
				}
				p.mu.Unlock()
				if d > 0 {
					time.Sleep(d)
				}
				if cut {
					p.cuts.Add(1)
					return // the stream dies mid-transfer
				}
				sent += int64(n)
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pipe(server, client, false)
	pipe(client, server, true)
}

// ---- helpers

// markerCmd appends a line to path each time it runs: a visible side
// effect, so "ran twice" is observable.
func markerCmd(path string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-Command", "Add-Content -Path '" + path + "' -Value x"}
	}
	return "sh", []string{"-c", `echo x >> '` + path + `'`}
}

func markerCount(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "x")
}

func chaosAgent(t *testing.T, ctx context.Context, addr, name string, slots int, backoff time.Duration) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn, WorkloadSlots: slots, OllamaURL: "127.0.0.1:1", Insecure: true,
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: backoff, MaxReconnectBackoff: 4 * backoff, HostFingerprint: "-",
		CapabilityProbeInterval: -1,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)
	return a
}

type chaosManager struct {
	srv  *manager.Server
	st   *persistent.Store
	stop func()
}

// startPersistentManager runs a manager on a database file; stop shuts it
// down the way a killed process would end (context cancelled, store closed).
func startPersistentManager(t *testing.T, addr, db string, mod func(*manager.Config)) *chaosManager {
	t.Helper()
	st, err := persistent.Open(db)
	if err != nil {
		t.Fatalf("persistent.Open: %v", err)
	}
	cfg := manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, ReconcileInterval: 100 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	srv := manager.NewServer(ws.New(), st, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	waitListening(t, addr)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			time.Sleep(200 * time.Millisecond) // let connection goroutines unwind before the file closes
			st.Close()
		})
	}
	t.Cleanup(stop)
	return &chaosManager{srv: srv, st: st, stop: stop}
}

func chaosState(srv *manager.Server, id domain.WorkloadID) domain.WorkloadState {
	rec, ok := srv.Workloads.Get(id)
	if !ok {
		return "MISSING"
	}
	return rec.Status.State
}

// ---- 1. a manager killed with queued, running and finished work

func TestChaosManagerRestartKeepsEveryWorkloadExactlyOnce(t *testing.T) {
	const addr = "127.0.0.1:19722"
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")
	m1 := startPersistentManager(t, addr, db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := chaosAgent(t, ctx, addr, "chaos-restart", 1, 100*time.Millisecond)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m1.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	sub := func(srv *manager.Server, target domain.NodeID, cmd string, args []string) domain.WorkloadID {
		c, cn := context.WithTimeout(context.Background(), 3*time.Second)
		defer cn()
		wl, err := srv.SubmitWorkload(c, target, cmd, args, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
		if err != nil {
			t.Fatalf("SubmitWorkload: %v", err)
		}
		return wl.ID
	}

	// Finished before the crash.
	doneMarker := filepath.Join(dir, "done.txt")
	c, args := markerCmd(doneMarker)
	doneID := sub(m1.srv, a.NodeID(), c, args)
	waitFor(t, 15*time.Second, func() bool { return chaosState(m1.srv, doneID) == domain.WorkloadCompleted })

	// Running at the crash (holds the agent's only slot) and three queued behind it.
	sc, sargs := sleepArgs("30")
	runningID := sub(m1.srv, a.NodeID(), sc, sargs)
	waitFor(t, 10*time.Second, func() bool { return chaosState(m1.srv, runningID) == domain.WorkloadRunning })
	var queued []domain.WorkloadID
	var queuedMarkers []string
	for i := 0; i < 3; i++ {
		mk := filepath.Join(dir, fmt.Sprintf("queued-%d.txt", i))
		c, args := markerCmd(mk)
		queued = append(queued, sub(m1.srv, "", c, args))
		queuedMarkers = append(queuedMarkers, mk)
	}
	for _, id := range queued {
		if s := chaosState(m1.srv, id); s != domain.WorkloadQueued {
			t.Fatalf("setup: %s is %s, want QUEUED behind the busy slot", id, s)
		}
	}

	// Kill the manager.
	m1.stop()
	restarted := time.Now()
	m2 := startPersistentManager(t, addr, db, nil)

	// The finished one is still finished, with its result.
	if s := chaosState(m2.srv, doneID); s != domain.WorkloadCompleted {
		t.Fatalf("finished workload came back %s", s)
	}
	// Every queued one runs, once, within a bound.
	waitFor(t, 30*time.Second, func() bool {
		for _, id := range queued {
			if chaosState(m2.srv, id) != domain.WorkloadCompleted {
				return false
			}
		}
		return true
	})
	t.Logf("queued work finished %.1fs after the restart", time.Since(restarted).Seconds())
	// The one that was running was interrupted (the agent drops work when the
	// manager goes away): it must not have been silently re-run or lost.
	switch s := chaosState(m2.srv, runningID); s {
	case domain.WorkloadUnknown, domain.WorkloadFailed, domain.WorkloadCanceled:
	default:
		t.Fatalf("interrupted workload is %s: want UNKNOWN, FAILED or CANCELED", s)
	}
	time.Sleep(500 * time.Millisecond)
	if markerCount(doneMarker) != 1 {
		t.Errorf("finished workload ran %d times, want 1", markerCount(doneMarker))
	}
	for i, mk := range queuedMarkers {
		if n := markerCount(mk); n != 1 {
			t.Errorf("queued workload %d ran %d times, want exactly 1", i, n)
		}
	}
}

// ---- 2. connections dropped during file transfers

func TestChaosTransferSurvivesDroppedConnections(t *testing.T) {
	const mgrAddr, proxyAddr = "127.0.0.1:19720", "127.0.0.1:19721"
	m := startArtifactManager(t, mgrAddr, false)
	p := newChaosProxy(t, proxyAddr, mgrAddr)
	p.mu.Lock()
	p.cutAfter, p.cutsLeft = 100<<10, 4 // 4 downloads die after 100 KiB: more than one attempt's retries
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startFileAgent(t, ctx, m, proxyAddr, "chaos-transfer")
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	payload := make([]byte, 512<<10)
	rand.NewChaCha8([32]byte{7}).Read(payload)
	sha := uploadArtifact(t, m.api, payload)
	cmd, args := "cp", []string{"in/data.bin", "out/copy.bin"}
	if runtime.GOOS == "windows" {
		cmd, args = "powershell", []string{"-NoProfile", "-Command", `Copy-Item in\data.bin out\copy.bin`}
	}
	code, out := postWorkload(t, m.api, map[string]any{
		"target": a.NodeID(), "command": cmd, "args": args,
		"inputs":        []map[string]string{{"name": "in/data.bin", "sha256": sha}},
		"outputs":       []string{"out/copy.bin"},
		"restartPolicy": "on-failure",
	})
	if code != http.StatusAccepted {
		t.Fatalf("POST /workloads: %d %v", code, out)
	}
	id := out["id"].(string)
	var v fileWorkloadView
	waitFor(t, 60*time.Second, func() bool {
		v = getFileWorkload(t, m.api, id)
		return v.State == domain.WorkloadCompleted
	})
	if got := p.cuts.Load(); got < 4 {
		t.Fatalf("proxy cut %d transfers, want 4: the fault was not exercised", got)
	}
	if len(v.OutputFiles) != 1 || v.OutputFiles[0].SHA256 != sha {
		t.Fatalf("output %+v does not carry the input's hash %s: the copy was damaged", v.OutputFiles, sha)
	}
	resp, err := http.Get(m.api + "/artifacts/" + sha)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, payload) {
		t.Fatal("stored file differs from what was uploaded")
	}
}

// ---- 3. the whole fleet reconnecting at once

func TestChaosManagerRestartHerdReconnectsWithinBoundAndSpread(t *testing.T) {
	const addr = "127.0.0.1:19723"
	const fleet = 24
	db := filepath.Join(t.TempDir(), "m.db")
	m1 := startPersistentManager(t, addr, db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var agents []*agent.Agent
	for i := 0; i < fleet; i++ {
		agents = append(agents, chaosAgent(t, ctx, addr, fmt.Sprintf("herd-%02d", i), 1, 400*time.Millisecond))
	}
	allReady := func(srv *manager.Server) int {
		n := 0
		for _, a := range agents {
			if rec, ok := srv.Registry.Get(a.NodeID()); ok && rec.State == domain.NodeReady {
				n++
			}
		}
		return n
	}
	waitFor(t, 30*time.Second, func() bool { return allReady(m1.srv) == fleet })

	m1.stop()
	down := time.Now()
	time.Sleep(300 * time.Millisecond)
	m2 := startPersistentManager(t, addr, db, nil)
	var firstSeen sync.Map
	waitFor(t, 30*time.Second, func() bool {
		for _, a := range agents {
			if rec, ok := m2.srv.Registry.Get(a.NodeID()); ok && rec.State == domain.NodeReady {
				firstSeen.LoadOrStore(a.NodeID(), time.Now())
			}
		}
		return allReady(m2.srv) == fleet
	})
	var first, last time.Time
	firstSeen.Range(func(_, v any) bool {
		ts := v.(time.Time)
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if ts.After(last) {
			last = ts
		}
		return true
	})
	t.Logf("%d agents back %.1fs after the kill; arrivals spread over %v", fleet, time.Since(down).Seconds(), last.Sub(first).Round(time.Millisecond))
	if time.Since(down) > 20*time.Second {
		t.Errorf("fleet took %v to come back", time.Since(down))
	}
	if last.Sub(first) < 100*time.Millisecond {
		t.Errorf("all %d agents reconnected within %v of each other: reconnects are not jittered", fleet, last.Sub(first))
	}
}

// ---- 4. a slow link that keeps dropping

func TestChaosLossyLinkNeverDuplicatesSideEffects(t *testing.T) {
	const mgrAddr, proxyAddr = "127.0.0.1:19724", "127.0.0.1:19725"
	dir := t.TempDir()
	m := startPersistentManager(t, mgrAddr, filepath.Join(dir, "m.db"), nil)
	p := newChaosProxy(t, proxyAddr, mgrAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := chaosAgent(t, ctx, proxyAddr, "chaos-lossy", 4, 100*time.Millisecond)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	p.setDelay(30 * time.Millisecond)
	var ids []domain.WorkloadID
	var markers []string
	stopChaos := make(chan struct{})
	go func() { // drop every connection every ~700ms for a while
		for {
			select {
			case <-stopChaos:
				return
			case <-time.After(700 * time.Millisecond):
				p.DropAll()
			}
		}
	}()
	for i := 0; i < 12; i++ {
		mk := filepath.Join(dir, fmt.Sprintf("lossy-%d.txt", i))
		c, args := markerCmd(mk)
		sc, cn := context.WithTimeout(context.Background(), 3*time.Second)
		wl, err := m.srv.SubmitWorkload(sc, "", c, args, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
		cn()
		if err != nil {
			continue // refused while the node was away: it was never accepted
		}
		ids = append(ids, wl.ID)
		markers = append(markers, mk)
		time.Sleep(250 * time.Millisecond)
	}
	close(stopChaos)
	p.setDelay(0)

	// Recovery: with the link healthy again, new work completes within a bound.
	waitFor(t, 20*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	mk := filepath.Join(dir, "after.txt")
	c, args := markerCmd(mk)
	var after domain.WorkloadID
	waitFor(t, 20*time.Second, func() bool {
		sc, cn := context.WithTimeout(context.Background(), 2*time.Second)
		defer cn()
		wl, err := m.srv.SubmitWorkload(sc, "", c, args, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
		after = wl.ID
		return err == nil
	})
	waitFor(t, 20*time.Second, func() bool { return chaosState(m.srv, after) == domain.WorkloadCompleted })

	time.Sleep(time.Second)
	completed := 0
	for i, id := range ids {
		n := markerCount(markers[i])
		if n > 1 {
			t.Errorf("workload %s ran %d times", id, n)
		}
		if chaosState(m.srv, id) == domain.WorkloadCompleted {
			completed++
			if n != 1 {
				t.Errorf("workload %s is COMPLETED but its side effect happened %d times", id, n)
			}
		}
	}
	t.Logf("%d of %d workloads completed through the chaos", completed, len(ids))
	if markerCount(mk) != 1 {
		t.Errorf("post-chaos workload ran %d times", markerCount(mk))
	}
}

// ---- 5. a full disk

func TestChaosFullDiskRefusesNewWorkKeepsServingAndRecovers(t *testing.T) {
	const addr = "127.0.0.1:19726"
	manager.StorageProbeEvery = 100 * time.Millisecond
	t.Cleanup(func() { manager.StorageProbeEvery = 5 * time.Second })
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")
	store, err := artifacts.Open(artifacts.Config{Dir: filepath.Join(dir, "art"), MaxBytes: 1 << 20, TotalBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	m := startPersistentManager(t, addr, db, func(c *manager.Config) { c.Artifacts = store })
	api := httptest.NewServer(m.srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := chaosAgent(t, ctx, addr, "chaos-disk", 2, 100*time.Millisecond)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	// Work accepted while the disk is fine, still running when it fills.
	sc, sargs := sleepArgs("3")
	c1, cn := context.WithTimeout(context.Background(), 3*time.Second)
	running, err := m.srv.SubmitWorkload(c1, a.NodeID(), sc, sargs, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
	cn()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return chaosState(m.srv, running.ID) == domain.WorkloadRunning })
	before := len(m.srv.Workloads.List())

	m.st.InjectWriteFault(persistent.ErrNoSpace)
	store.InjectWriteFault(persistent.ErrNoSpace)
	// The first write that fails is what reveals the full disk.
	cmd, args := echoArgs("refused")
	body := fmt.Sprintf(`{"command":%q,"args":[%q,%q]}`, cmd, args[0], args[len(args)-1])
	post := func() (*http.Response, string) {
		r, err := http.Post(api.URL+"/workloads", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}
	// A write that was already in flight when the fault began may still succeed
	// and clear the flag; the next failing write sets it again, as in life.
	failingWrite := func() { m.st.UpsertJob(domain.Job{ID: "probe"}) }
	failingWrite()
	failingWrite()
	resp, text := post()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("submission on a full disk: %s %q, want 507", resp.Status, text)
	}
	if !strings.Contains(text, "disk") {
		t.Errorf("the refusal should say why: %q", text)
	}
	if r, err := http.Post(api.URL+"/artifacts", "application/octet-stream", strings.NewReader("data")); err != nil || r.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("artifact upload on a full disk: %v %v", r, err)
	}
	// Reads and the running work carry on.
	var nodes []map[string]any
	getJSON(t, api.URL+"/nodes", &nodes)
	if len(nodes) != 1 {
		t.Errorf("GET /nodes while degraded returned %d nodes", len(nodes))
	}
	failingWrite()
	var rel struct {
		Storage struct {
			Degraded bool `json:"degraded"`
		} `json:"storage"`
	}
	getJSON(t, api.URL+"/reliability", &rel)
	if !rel.Storage.Degraded {
		t.Errorf("/reliability does not report the degraded storage (store health %+v, artifacts %+v)", m.st.Health(), store.Health())
	}
	waitFor(t, 15*time.Second, func() bool { return chaosState(m.srv, running.ID) == domain.WorkloadCompleted })
	if got := len(m.srv.Workloads.List()); got != before {
		t.Errorf("workloads grew from %d to %d while the disk was full: refused work must not be accepted", before, got)
	}

	// Space returns: recovery needs nothing but time.
	m.st.InjectWriteFault(nil)
	store.InjectWriteFault(nil)
	freed := time.Now()
	waitFor(t, 10*time.Second, func() bool {
		r, _ := post()
		return r.StatusCode == http.StatusAccepted
	})
	t.Logf("accepting work again %v after space returned", time.Since(freed).Round(time.Millisecond))

	// What was accepted is durable: reopen the database.
	m.stop()
	st2, err := persistent.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	pw, _ := st2.ListWorkloads()
	found := false
	for _, w := range pw {
		found = found || w.Workload.ID == running.ID
	}
	if !found {
		t.Error("the workload accepted before the disk filled is missing after a restart")
	}
}

// ---- 6. a clean drain

func TestChaosDrainRefusesNewWorkLetsRunningFinishAndDisconnects(t *testing.T) {
	const addr = "127.0.0.1:19727"
	m := startPersistentManager(t, addr, filepath.Join(t.TempDir(), "m.db"), nil)
	api := httptest.NewServer(m.srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := chaosAgent(t, ctx, addr, "chaos-drain", 2, 100*time.Millisecond)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	sc, sargs := sleepArgs("2")
	c1, cn := context.WithTimeout(context.Background(), 3*time.Second)
	wl, err := m.srv.SubmitWorkload(c1, a.NodeID(), sc, sargs, "", nil, domain.ResourceRequirements{}, domain.RestartNever)
	cn()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return chaosState(m.srv, wl.ID) == domain.WorkloadRunning })

	drained := make(chan time.Duration, 1)
	go func() { t0 := time.Now(); m.srv.Drain(15 * time.Second); drained <- time.Since(t0) }()
	waitFor(t, 3*time.Second, m.srv.Draining)
	r, err := http.Post(api.URL+"/workloads", "application/json", strings.NewReader(`{"command":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("submission while draining: %s, want 503", r.Status)
	}
	select {
	case d := <-drained:
		if d > 10*time.Second {
			t.Errorf("drain took %v", d)
		}
		if s := chaosState(m.srv, wl.ID); s != domain.WorkloadCompleted {
			t.Errorf("running workload was %s at the end of the drain, want COMPLETED", s)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("drain did not finish within its bound")
	}
	// Devices were told: their connections are closed, so the node is no longer READY.
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State != domain.NodeReady
	})
}

// ---- 7. a drain that times out must not count as the devices' failure

func TestChaosDrainTimeoutLeavesRunningWorkUnknownNotFailed(t *testing.T) {
	const addr = "127.0.0.1:19728"
	db := filepath.Join(t.TempDir(), "m.db")
	m := startPersistentManager(t, addr, db, nil)
	api := httptest.NewServer(m.srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := chaosAgent(t, ctx, addr, "chaos-drain-timeout", 2, 100*time.Millisecond)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
	sc, sargs := sleepArgs("30")
	b, _ := json.Marshal(map[string]any{"maxAttempts": 3, "tasks": []map[string]any{{"command": sc, "args": sargs}}})
	r, err := http.Post(api.URL+"/jobs", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	waitFor(t, 10*time.Second, func() bool {
		for _, w := range m.srv.Workloads.List() {
			if w.Status.State == domain.WorkloadRunning {
				return true
			}
		}
		return false
	})

	m.srv.Drain(time.Second) // work is still running when the wait ends
	time.Sleep(time.Second)  // the connection-closed handling has run by now
	n := 0
	for _, w := range m.srv.Workloads.List() {
		n++
		if w.Status.State == domain.WorkloadFailed {
			t.Errorf("workload %s was recorded FAILED by the drain: that would cost the job an attempt", w.Workload.ID)
		}
	}
	if n != 1 {
		t.Errorf("%d workloads exist after the drain, want 1 (no retry attempt may have been created)", n)
	}

	m.stop()
	m2 := startPersistentManager(t, addr, db, nil)
	for _, w := range m2.srv.Workloads.List() {
		if w.Status.State != domain.WorkloadUnknown {
			t.Errorf("after restart workload %s is %s, want UNKNOWN", w.Workload.ID, w.Status.State)
		}
	}
}

var _ = errors.New
