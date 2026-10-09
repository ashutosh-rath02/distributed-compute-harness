package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/metrics"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

const testMetricsToken = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a" // testAIKey: openai_test.go

type obsManager struct {
	srv *manager.Server
	api string
	ids []domain.NodeID
}

// startObsManager runs a real manager (database, artifact store, all three
// credentials) with two real agents.
func startObsManager(t *testing.T, addr string) obsManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, err := persistent.Open(filepath.Join(t.TempDir(), "obs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := manager.NewServer(ws.New(), store, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second, ReconcileInterval: 100 * time.Millisecond,
		Artifacts: openArtifactStore(t, 1<<20), OperatorToken: operatorToken, AIKey: testAIKey, MetricsToken: testMetricsToken,
	})
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	m := obsManager{srv: srv, api: api.URL}
	// Agents need the artifact route on the agent transport only for file
	// jobs; the manager under test has no transport handle, so these tests
	// use jobs without files.
	for _, name := range []string{"obs-a", "obs-b"} {
		a := startRegisteredAgent(t, addr, name)
		m.ids = append(m.ids, a.NodeID())
	}
	waitFor(t, 10*time.Second, func() bool {
		for _, id := range m.ids {
			rec, ok := srv.Registry.Get(id)
			if !ok || rec.State != domain.NodeReady || rec.LastMetrics.LastHeartbeat.IsZero() {
				return false
			}
		}
		return true
	})
	return m
}

func authed(t *testing.T, method, url, bearer, body string) (*http.Response, []byte) {
	t.Helper()
	resp := operatorCall(t, method, url, bearer, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func scrape(t *testing.T, api, bearer string) *metrics.Exposition {
	t.Helper()
	resp, body := authed(t, http.MethodGet, api+"/metrics", bearer, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", ct)
	}
	e, err := metrics.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("/metrics is not valid exposition format: %v", err)
	}
	return e
}

func failCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-Command", "exit 3"}
	}
	return "sh", []string{"-c", "exit 3"}
}

func TestMetricsAfterAJobAndAFailure(t *testing.T) {
	m := startObsManager(t, "127.0.0.1:19710")

	// A job of three tasks (no files) and one workload that fails.
	cmd, _ := failCommand()
	okCmd, okArgs := "sh", []string{"-c", "exit 0"}
	if runtime.GOOS == "windows" {
		okCmd, okArgs = "powershell", []string{"-NoProfile", "-Command", "exit 0"}
	}
	var tasks []map[string]any
	for i := 0; i < 3; i++ {
		tasks = append(tasks, map[string]any{"name": fmt.Sprintf("t%d", i), "command": okCmd, "args": okArgs})
	}
	jb, _ := json.Marshal(map[string]any{"name": "obs", "tasks": tasks})
	resp, body := authed(t, http.MethodPost, m.api+"/jobs", operatorToken, string(jb))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /jobs: %d %s", resp.StatusCode, body)
	}
	requestID := resp.Header.Get("X-Request-Id")
	if len(requestID) != 16 {
		t.Fatalf("X-Request-Id %q", requestID)
	}
	var job jobJSON
	json.Unmarshal(body, &job)
	_, fargs := failCommand()
	wb, _ := json.Marshal(map[string]any{"command": cmd, "args": fargs})
	if resp, body = authed(t, http.MethodPost, m.api+"/workloads", operatorToken, string(wb)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /workloads: %d %s", resp.StatusCode, body)
	}
	if other := operatorCall(t, http.MethodGet, m.api+"/nodes", operatorToken, ""); other.Header.Get("X-Request-Id") == requestID || other.Header.Get("X-Request-Id") == "" {
		t.Fatalf("request IDs must be unique per request")
	}
	waitFor(t, 60*time.Second, func() bool {
		e := scrape(t, m.api, operatorToken)
		return e.Sum("harness_workloads_finished_total") >= 4 && e.Sum("harness_agent_workloads_run_total") >= 4
	})

	e := scrape(t, m.api, operatorToken)
	cap := "system.execute"
	check := func(want float64, name string, kv ...string) {
		t.Helper()
		if got, ok := e.Get(name, kv...); !ok || got != want {
			t.Errorf("%s%v = %v (present %v), want %v", name, kv, got, ok, want)
		}
	}
	check(2, "harness_nodes", "state", "READY")
	check(2, "harness_nodes_taking_work")
	check(0, "harness_nodes_held_back", "reason", "paused")
	check(3, "harness_workloads_finished_total", "capability", cap, "outcome", "completed")
	check(1, "harness_workloads_finished_total", "capability", cap, "outcome", "failed")
	check(3, "harness_workload_duration_seconds_count", "capability", cap, "outcome", "completed")
	check(1, "harness_workload_duration_seconds_count", "capability", cap, "outcome", "failed")
	check(4, "harness_workload_queue_wait_seconds_count", "capability", cap)
	check(1, "harness_jobs_finished_total", "state", "COMPLETED")
	check(1, "harness_jobs", "state", "COMPLETED")
	check(0, "harness_queue_depth")
	check(3, "harness_workloads", "state", "COMPLETED")
	check(1, "harness_workloads", "state", "FAILED")
	check(2, "harness_node_registrations_total", "kind", "new")
	check(1, "harness_http_requests_total", "route", "POST /jobs", "code", "2xx")
	check(1, "harness_audit_events_total", "log", "security", "kind", "job.submitted")
	check(1, "harness_ready")
	check(0, "harness_failover_stepped_down")
	if v := e.Sum("harness_agent_heartbeats_total"); v < 2 {
		t.Errorf("heartbeats: %v", v)
	}
	if v, _ := e.Get("harness_store_size_bytes"); v <= 0 {
		t.Errorf("store size %v", v)
	}
	if v := e.Sum("harness_agent_workloads_failed_total"); v != 1 {
		t.Errorf("agents report %v failed workloads, want 1", v)
	}
	if v := e.Sum("harness_agent_uptime_seconds"); v < 0 {
		t.Errorf("uptime %v", v)
	}
	if e.Series("harness_build_info") != 1 {
		t.Errorf("build info series: %d", e.Series("harness_build_info"))
	}
	if e.Series("harness_node_heartbeat_age_seconds") != 2 {
		t.Errorf("per-node series: %d", e.Series("harness_node_heartbeat_age_seconds"))
	}

	// The request ID is in the audit entry the request wrote.
	entries, err := m.srv.AuditEntries(domain.AuditSecurity, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, en := range entries {
		if en.Kind == "job.submitted" && en.Detail["requestId"] == requestID {
			found = true
		}
	}
	if !found {
		t.Errorf("no job.submitted audit entry carries request ID %s", requestID)
	}

	// GET /nodes/{id} shows the agent's own counters; GET /status summarises.
	var nv struct {
		Self *domain.AgentSelfMetrics `json:"self"`
	}
	_, nb := authed(t, http.MethodGet, m.api+"/nodes/"+string(m.ids[0]), operatorToken, "")
	json.Unmarshal(nb, &nv)
	if nv.Self == nil || nv.Self.UptimeSeconds < 0 {
		t.Errorf("node view lacks self metrics: %s", nb)
	}
	var st struct {
		Ready bool `json:"ready"`
		Nodes struct {
			Total, TakingWork int
		} `json:"nodes"`
		RecentFailures []struct{ Workload, Capability, Error string } `json:"recentFailures"`
	}
	_, sb := authed(t, http.MethodGet, m.api+"/status", operatorToken, "")
	if err := json.Unmarshal(sb, &st); err != nil || !st.Ready || st.Nodes.Total != 2 || len(st.RecentFailures) != 1 || st.RecentFailures[0].Capability != cap {
		t.Errorf("status: %s (%v)", sb, err)
	}
}

func TestMetricsAuthentication(t *testing.T) {
	m := startObsManager(t, "127.0.0.1:19711")
	code := func(method, path, bearer string) int {
		resp, _ := authed(t, method, m.api+path, bearer, "")
		return resp.StatusCode
	}
	for name, tc := range map[string]struct {
		method, path, bearer string
		want                 int
	}{
		"no token":                {"GET", "/metrics", "", 401},
		"wrong token":             {"GET", "/metrics", "nope", 401},
		"operator token":          {"GET", "/metrics", operatorToken, 200},
		"metrics token":           {"GET", "/metrics", testMetricsToken, 200},
		"ai key on metrics":       {"GET", "/metrics", testAIKey, 401},
		"metrics token on nodes":  {"GET", "/nodes", testMetricsToken, 401},
		"metrics token on status": {"GET", "/status", testMetricsToken, 401},
		"metrics token on audit":  {"GET", "/audit", testMetricsToken, 401},
		"metrics token on join":   {"GET", "/join-info", testMetricsToken, 401},
		"metrics token POST":      {"POST", "/metrics", testMetricsToken, 401},
		"metrics token on submit": {"POST", "/workloads", testMetricsToken, 401},
		"metrics token on v1":     {"GET", "/v1/models", testMetricsToken, 401},
		"ai key on status":        {"GET", "/status", testAIKey, 401},
		"healthz, no token":       {"GET", "/healthz", "", 200},
		"readyz, no token":        {"GET", "/readyz", "", 200},
		"healthz, metrics token":  {"GET", "/healthz", testMetricsToken, 200},
		"readyz, wrong token":     {"GET", "/readyz", "nope", 200}, // public: the token is not consulted
	} {
		if got := code(tc.method, tc.path, tc.bearer); got != tc.want {
			t.Errorf("%s: %s %s -> %d, want %d", name, tc.method, tc.path, got, tc.want)
		}
	}
	// What the probes say is minimal and free of secrets.
	for path, want := range map[string]string{"/healthz": "ok\n", "/readyz": "ready\n"} {
		if _, b := authed(t, http.MethodGet, m.api+path, "", ""); string(b) != want {
			t.Errorf("%s body %q", path, b)
		}
	}
	// Nothing the scraper can read contains a credential.
	resp, body := authed(t, http.MethodGet, m.api+"/metrics", testMetricsToken, "")
	for _, secret := range []string{operatorToken, testAIKey, testMetricsToken, pairingToken} {
		if strings.Contains(string(body), secret) {
			t.Errorf("/metrics leaks a credential")
		}
	}
	_ = resp
}

func TestMetricsCardinalityStaysBounded(t *testing.T) {
	m := startObsManager(t, "127.0.0.1:19712")
	batch := func(from, to int) {
		for i := from; i < to; i++ {
			id := fmt.Sprintf("wl-id-%04d-aaaaaaaaaaaaaaaa", i)
			sha := fmt.Sprintf("%064x", i+1)
			authed(t, "GET", m.api+"/workloads/"+id, operatorToken, "")
			authed(t, "GET", m.api+"/artifacts/"+sha+"?name=secret-file-"+fmt.Sprint(i)+".txt", operatorToken, "")
			authed(t, "GET", m.api+"/no-such-route-"+id, operatorToken, "")
			authed(t, "POST", m.api+"/workloads/"+id+"/cancel", operatorToken, "")
			authed(t, "POST", m.api+"/artifacts", operatorToken, "file "+id)
		}
	}
	// The first batch meets each route once; after that, 900 more distinct
	// IDs and file names must not add a single series.
	batch(0, 100)
	scrape(t, m.api, operatorToken) // the scrape route itself appears after its first scrape
	before := scrape(t, m.api, operatorToken)
	batch(100, 1000)
	resp, body := authed(t, "GET", m.api+"/metrics", operatorToken, "")
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	for _, leak := range []string{"wl-id-", "secret-file", "no-such-route"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("user-controlled text %q reached /metrics", leak)
		}
	}
	after := scrape(t, m.api, operatorToken)
	if grew := len(after.Samples) - len(before.Samples); grew != 0 {
		have := map[string]bool{}
		for _, s := range before.Samples {
			have[fmt.Sprint(s.Name, s.Labels)] = true
		}
		for _, s := range after.Samples {
			if !have[fmt.Sprint(s.Name, s.Labels)] {
				t.Logf("new series: %s %v", s.Name, s.Labels)
			}
		}
		t.Fatalf("4500 requests with 900 new IDs and file names grew the exposition by %d series", grew)
	}
	if after.Sum("harness_http_requests_total", "route", "unmatched") < 1000 {
		t.Errorf("unknown paths must be counted together as route=unmatched")
	}
	if len(body) > 64<<10 {
		t.Errorf("/metrics is %d bytes", len(body))
	}
}
