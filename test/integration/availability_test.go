package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

// fakeUse is a device's use that the test changes as it goes.
type fakeUse struct {
	mu  sync.Mutex
	use domain.DeviceUse
}

func (f *fakeUse) set(onBattery bool, idleSeconds int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pct := 40
	f.use = domain.DeviceUse{OnBattery: &onBattery, BatteryPercent: &pct, IdleSeconds: &idleSeconds}
}

func (f *fakeUse) read(context.Context) domain.DeviceUse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.use
}

func startUseAgent(t *testing.T, ctx context.Context, addr, name string, use *fakeUse, ollamaURL string) *agent.Agent {
	t.Helper()
	if ollamaURL == "" {
		ollamaURL = "127.0.0.1:1"
	}
	a, err := agent.New(ws.New(), agent.Config{
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond,
		HostFingerprint: "-", Insecure: true, WorkloadSlots: 2, OllamaURL: ollamaURL, CapabilityProbeInterval: 200 * time.Millisecond, DeviceUse: use.read,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	return a
}

func putAvailability(t *testing.T, api string, id domain.NodeID, rule map[string]any) {
	t.Helper()
	b, _ := json.Marshal(rule)
	req, _ := http.NewRequest(http.MethodPut, api+"/nodes/"+string(id)+"/availability", bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT availability %v: %s", rule, resp.Status)
	}
}

type nodeAvailability struct {
	Availability struct {
		Rule      domain.Availability `json:"rule"`
		Available bool                `json:"available"`
		Reason    string              `json:"reason"`
	} `json:"availability"`
}

// A laptop on battery takes no new work: it waits, saying why, and
// starts the moment the laptop is plugged in. Work already running when
// it is unplugged finishes.
func TestWorkWaitsForAPluggedInDeviceAndRunningWorkFinishes(t *testing.T) {
	const addr = "127.0.0.1:19593"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	use := &fakeUse{}
	use.set(true, 0) // unplugged, from the moment it connects
	a := startUseAgent(t, ctx, addr, "laptop", use, "")
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	code, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"})
	if code != http.StatusAccepted || out["state"] != "QUEUED" {
		t.Fatalf("on battery: %d %v, want QUEUED", code, out)
	}
	id := out["id"].(string)
	var v map[string]any
	getJSON(t, m.api+"/workloads/"+id, &v)
	if v["waiting"] != "laptop: on battery (40%)" {
		t.Fatalf("waiting reason %q", v["waiting"])
	}
	var n nodeAvailability
	getJSON(t, m.api+"/nodes/"+string(a.NodeID()), &n)
	if n.Availability.Available || n.Availability.Reason != "on battery (40%)" || n.Availability.Rule.Mode != domain.AvailableAuto {
		t.Fatalf("node availability %+v", n.Availability)
	}
	// Asking for this device by name doesn't get around it either.
	code, out = postWorkload(t, m.api, map[string]any{"capability": "system.identity", "target": a.NodeID()})
	if code != http.StatusAccepted || out["state"] != "QUEUED" {
		t.Fatalf("pinned to it on battery: %d %v, want QUEUED", code, out)
	}
	pinned := out["id"].(string)
	time.Sleep(500 * time.Millisecond)
	if getJSON(t, m.api+"/workloads/"+id, &v); v["state"] != "QUEUED" {
		t.Fatalf("ran on battery: %v", v["state"])
	}

	start := time.Now()
	use.set(false, 0) // plugged in
	for _, w := range []string{id, pinned} {
		if v := waitState(t, m.api, w, 10*time.Second); v["state"] != "COMPLETED" || time.Since(start) > 3*time.Second {
			t.Fatalf("after plugging in: %v after %v", v["state"], time.Since(start))
		}
	}

	// Running when it is unplugged: drains, never killed.
	_, out = postWorkload(t, m.api, map[string]any{"capability": "cpu.burn", "params": map[string]string{"seconds": "2"}})
	burn := out["id"].(string)
	waitFor(t, 5*time.Second, func() bool { getJSON(t, m.api+"/workloads/"+burn, &v); return v["state"] == "RUNNING" })
	use.set(true, 0)
	waitFor(t, 3*time.Second, func() bool {
		getJSON(t, m.api+"/nodes/"+string(a.NodeID()), &n)
		return !n.Availability.Available
	})
	_, out = postWorkload(t, m.api, map[string]any{"capability": "system.identity"})
	if out["state"] != "QUEUED" {
		t.Fatalf("new work after unplugging: %v", out["state"])
	}
	if v := waitState(t, m.api, burn, 10*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("running work after unplugging: %v (%v)", v["state"], v["error"])
	}
}

// The operator's rule: idle, paused, hours; and a phone-style device
// that reconnects on battery gets nothing in the gap after connecting.
func TestAvailabilityRulesFromTheOperator(t *testing.T) {
	const addr = "127.0.0.1:19594"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	use := &fakeUse{}
	use.set(false, 30) // plugged in, someone at the keyboard 30 s ago
	a := startUseAgent(t, ctx, addr, "desk", use, "")
	id := a.NodeID()
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })

	putAvailability(t, m.api, id, map[string]any{"mode": "idle", "idleMinutes": 2})
	_, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"})
	wid := out["id"].(string)
	var v map[string]any
	getJSON(t, m.api+"/workloads/"+wid, &v)
	if out["state"] != "QUEUED" || v["waiting"] != "desk: in use (idle 30s, needs 2 min)" {
		t.Fatalf("idle rule while in use: %v, waiting %q", out["state"], v["waiting"])
	}
	use.set(false, 150) // nobody for 2.5 minutes
	if v := waitState(t, m.api, wid, 10*time.Second); v["state"] != "COMPLETED" {
		t.Fatalf("after the owner left: %v", v["state"])
	}

	putAvailability(t, m.api, id, map[string]any{"mode": "paused"})
	if _, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"}); out["state"] != "QUEUED" {
		t.Fatalf("paused: %v", out["state"])
	}
	// A window that excludes now.
	now := time.Now()
	from, to := now.Add(2*time.Hour).Format("15:04"), now.Add(3*time.Hour).Format("15:04")
	putAvailability(t, m.api, id, map[string]any{"mode": "always", "hours": from + "-" + to})
	var n nodeAvailability
	getJSON(t, m.api+"/nodes/"+string(id), &n)
	if n.Availability.Available || !strings.Contains(n.Availability.Reason, "outside its hours") {
		t.Fatalf("outside the window: %+v", n.Availability)
	}
	// Back to always: the paused one starts right away.
	putAvailability(t, m.api, id, map[string]any{"mode": "always"})
	waitFor(t, 5*time.Second, func() bool {
		for _, rec := range m.srv.Workloads.List() {
			if rec.Status.State == domain.WorkloadQueued {
				return false
			}
		}
		return true
	})

	// Bad rules are refused.
	b, _ := json.Marshal(map[string]any{"mode": "idle", "hours": "25:00-26:00"})
	req, _ := http.NewRequest(http.MethodPut, m.api+"/nodes/"+string(id)+"/availability", bytes.NewReader(b))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad hours: %s", resp.Status)
	}
}

// The rule survives a manager restart and a rename, and the device's
// reconnect on battery doesn't let queued work through.
func TestAvailabilityRuleSurvivesRestartAndReconnect(t *testing.T) {
	const addr = "127.0.0.1:19595"
	dbPath := filepath.Join(t.TempDir(), "manager.db")
	start := func() (artifactManager, context.CancelFunc) {
		store, err := persistent.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		transport := ws.New()
		srv := manager.NewServer(transport, store, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, ReconcileInterval: 100 * time.Millisecond})
		done := make(chan struct{})
		go func() { srv.Run(ctx); close(done) }()
		waitListening(t, addr)
		api := httptest.NewServer(srv.NewHTTPHandler())
		return artifactManager{srv: srv, api: api.URL}, func() { api.Close(); cancel(); <-done; store.Close() }
	}
	m, stop := start()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	use := &fakeUse{}
	use.set(true, 0)
	a := startUseAgent(t, ctx, addr, "phone", use, "")
	id := a.NodeID()
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	putAvailability(t, m.api, id, map[string]any{"mode": "charging"})
	b, _ := json.Marshal(map[string]any{"alias": "My phone"})
	req, _ := http.NewRequest(http.MethodPut, m.api+"/nodes/"+string(id)+"/meta", bytes.NewReader(b))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: %v %v", err, resp)
	}
	stop()

	m, stop = start()
	defer stop()
	waitFor(t, 10*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	var n nodeAvailability
	getJSON(t, m.api+"/nodes/"+string(id), &n)
	if n.Availability.Rule.Mode != domain.AvailableWhenCharging || n.Availability.Available || n.Availability.Reason != "on battery (40%)" {
		t.Fatalf("after a restart and a rename: %+v", n.Availability)
	}
	// Reported on connecting: queued work isn't sent in the gap before
	// the first heartbeat.
	_, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"})
	wid := out["id"].(string)
	time.Sleep(700 * time.Millisecond)
	var v map[string]any
	if getJSON(t, m.api+"/workloads/"+wid, &v); v["state"] != "QUEUED" || v["waiting"] != "My phone: on battery (40%)" {
		t.Fatalf("on battery after a reconnect: %v, waiting %q", v["state"], v["waiting"])
	}
}

// Work pinned to a device that is held back waits for it, but doesn't hold
// up the same kind of work that any other device can take.
func TestPinnedWorkOnAnUnavailableDeviceDoesntBlockOthers(t *testing.T) {
	const addr = "127.0.0.1:19597"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	phoneUse, deskUse := &fakeUse{}, &fakeUse{}
	phoneUse.set(true, 0)
	deskUse.set(false, 0)
	phone := startUseAgent(t, ctx, addr, "phone", phoneUse, "")
	desk := startUseAgent(t, ctx, addr, "desk", deskUse, "")
	for _, a := range []*agent.Agent{phone, desk} {
		id := a.NodeID()
		waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	}
	putAvailability(t, m.api, phone.NodeID(), map[string]any{"mode": "paused"})
	_, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity", "target": phone.NodeID()})
	pinned := out["id"].(string)
	if out["state"] != "QUEUED" {
		t.Fatalf("pinned to a paused device: %v", out["state"])
	}
	var ids []string
	for i := 0; i < 3; i++ {
		_, out := postWorkload(t, m.api, map[string]any{"capability": "system.identity"})
		ids = append(ids, out["id"].(string))
	}
	for _, id := range ids {
		if v := waitState(t, m.api, id, 10*time.Second); v["state"] != "COMPLETED" || v["target"] != string(desk.NodeID()) {
			t.Fatalf("unpinned work behind a pinned one: %v on %v", v["state"], v["target"])
		}
	}
	var v map[string]any
	if getJSON(t, m.api+"/workloads/"+pinned, &v); v["state"] != "QUEUED" || v["waiting"] != "phone: paused" {
		t.Fatalf("the pinned one: %v, waiting %q", v["state"], v["waiting"])
	}
	putAvailability(t, m.api, phone.NodeID(), map[string]any{"mode": "always"})
	if v := waitState(t, m.api, pinned, 10*time.Second); v["state"] != "COMPLETED" || v["target"] != string(phone.NodeID()) {
		t.Fatalf("pinned, once its device takes work: %v on %v", v["state"], v["target"])
	}
}

// A chat whose only device is unplugged gets an immediate answer, not a
// request that hangs in the queue.
func TestChatToAnUnavailableDeviceFailsFast(t *testing.T) {
	const addr = "127.0.0.1:19596"
	m := startAIManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	use := &fakeUse{}
	use.set(true, 0)
	a := startUseAgent(t, ctx, addr, "laptop", use, startFakeOllama(t, &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"hi"}}))
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && rec.HasCapability("llm.chat")
	})
	start := time.Now()
	resp := openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("llama3.2", false, "hello"))
	var e struct {
		Error struct{ Code, Message string }
	}
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || e.Error.Code != "device_unavailable" ||
		!strings.Contains(e.Error.Message, "laptop: on battery (40%)") || time.Since(start) > 2*time.Second {
		t.Fatalf("chat on battery: %d %+v after %v", resp.StatusCode, e, time.Since(start))
	}
	use.set(false, 0)
	waitFor(t, 3*time.Second, func() bool { // (this manager's API needs credentials)
		rec, _ := m.srv.Registry.Get(a.NodeID())
		u := rec.LastMetrics.Use
		return u != nil && u.OnBattery != nil && !*u.OnBattery
	})
	resp = openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("llama3.2", false, "hello"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat plugged in: %d", resp.StatusCode)
	}
}
