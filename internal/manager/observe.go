package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/metrics"
)

// Observability (docs/observability.md): GET /metrics, /healthz, /readyz,
// a request ID per API request, and the instruments the rest of the manager
// calls. Everything here is nil-safe on *serverMetrics, so a Server built
// without NewServer (some tests) simply records nothing.
//
// Label rules, which keep the cardinality bounded: only capability types,
// states, route patterns, status classes and code-defined kinds are ever
// labels. Node IDs appear only in the per-node gauges, capped at
// maxNodeSeries. Never a workload ID, a file name or any user text.

// Version and Commit identify this build in harness_build_info. cmd/manager
// sets them (from internal/version when that exists, else -ldflags
// "-X home-harness/internal/manager.Version=..."); unset, Commit falls back
// to the VCS revision Go embedded in the binary.
var (
	Version = "dev"
	Commit  = ""
)

func buildCommit() string {
	if Commit != "" {
		return Commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return "unknown"
}

// maxNodeSeries caps the per-node gauges: the fleet is a home, so this is
// generous, and the cap keeps a flood of fake identities from blowing up a
// scrape (the first N by node ID are shown; the aggregates cover everyone).
const maxNodeSeries = 50

var (
	durationBuckets = []float64{0.05, 0.25, 1, 5, 15, 60, 300, 900, 3600}
	waitBuckets     = []float64{0.01, 0.1, 1, 5, 30, 120, 600, 3600, 21600}
	latencyBuckets  = []float64{0.001, 0.005, 0.025, 0.1, 0.5, 2.5, 10, 60}
)

type serverMetrics struct {
	reg   *metrics.Registry
	start time.Time

	heartbeats    metrics.Counter
	registrations *metrics.CounterVec
	rejections    *metrics.CounterVec
	offline       *metrics.CounterVec

	finished  *metrics.CounterVec
	timeouts  *metrics.CounterVec
	retries   *metrics.CounterVec
	duration  *metrics.HistogramVec
	queueWait *metrics.HistogramVec

	jobsDone      *metrics.CounterVec
	workflowsDone *metrics.CounterVec
	spotChecks    *metrics.CounterVec

	httpReqs     *metrics.CounterVec
	httpDuration *metrics.HistogramVec
	artifactIO   *metrics.CounterVec
	auditEvents  *metrics.CounterVec

	// A scrape asks for the fleet and the workloads from many families;
	// they share one pass for a second.
	cacheMu   sync.Mutex
	fleetAt   time.Time
	fleetSnap fleetSnapshot
	recsAt    time.Time
	recsSnap  []WorkloadRecord

	sse      atomic.Int64
	listen   atomic.Bool  // transports are bound (Run)
	lastTick atomic.Int64 // unix nanos of the last reconcile tick
}

func newServerMetrics(s *Server) *serverMetrics {
	r := metrics.NewRegistry()
	m := &serverMetrics{reg: r, start: time.Now()}

	m.heartbeats = r.NewCounterVec("harness_agent_heartbeats_total", "Heartbeats received from agents.").With()
	m.registrations = r.NewCounterVec("harness_node_registrations_total", "Agents that registered, by kind (new device or reconnect).", "kind")
	m.rejections = r.NewCounterVec("harness_rejections_total", "Refused registrations and sign-ins, by kind.", "kind")
	m.offline = r.NewCounterVec("harness_node_offline_total", "Nodes that went offline, by reason.", "reason")

	m.finished = r.NewCounterVec("harness_workloads_finished_total", "Workloads that reached a final state, by capability type and outcome.", "capability", "outcome")
	m.timeouts = r.NewCounterVec("harness_workload_timeouts_total", "Workloads that hit their time limit, by capability type.", "capability")
	m.retries = r.NewCounterVec("harness_workload_retries_total", "Workloads started again, by reason (busy_refusal, restart, job_attempt).", "reason")
	m.duration = r.NewHistogramVec("harness_workload_duration_seconds", "Time a workload ran on its device, by capability type and outcome.", durationBuckets, "capability", "outcome")
	m.queueWait = r.NewHistogramVec("harness_workload_queue_wait_seconds", "Time from being accepted to being handed to a device, by capability type.", waitBuckets, "capability")

	m.jobsDone = r.NewCounterVec("harness_jobs_finished_total", "Batch jobs that finished, by state.", "state")
	m.workflowsDone = r.NewCounterVec("harness_workflows_finished_total", "Workflows that finished, by state.", "state")
	m.spotChecks = r.NewCounterVec("harness_spot_checks_total", "Spot checks settled, by result (matched, mismatch, unresolved, skipped).", "result")

	m.httpReqs = r.NewCounterVec("harness_http_requests_total", "Operator API requests, by route pattern and status class.", "route", "code").SetMaxSeries(400)
	m.httpDuration = r.NewHistogramVec("harness_http_request_duration_seconds", "Operator API request latency, by route pattern (the event stream is excluded).", latencyBuckets, "route").SetMaxSeries(200)
	m.artifactIO = r.NewCounterVec("harness_artifact_bytes_total", "Bytes moved through the artifact store, by direction and who moved them (operator or agent).", "direction", "via")
	m.auditEvents = r.NewCounterVec("harness_audit_events_total", "Audit log entries written, by log and kind.", "log", "kind").SetMaxSeries(300)

	m.registerCollectors(s)
	return m
}

// ---- nil-safe recording helpers (called from the manager's paths)

func (m *serverMetrics) heartbeat() {
	if m != nil {
		m.heartbeats.Inc()
	}
}

func (m *serverMetrics) registered(isNew bool) {
	if m != nil {
		kind := "reconnect"
		if isNew {
			kind = "new"
		}
		m.registrations.With(kind).Inc()
	}
}

func (m *serverMetrics) rejected(kind string) {
	if m != nil {
		m.rejections.With(kind).Inc()
	}
}

func (m *serverMetrics) nodeOffline(reason string) {
	if m != nil {
		m.offline.With(reason).Inc()
	}
}

func (m *serverMetrics) tick() {
	if m != nil {
		m.lastTick.Store(time.Now().UnixNano())
	}
}

func (m *serverMetrics) listening() {
	if m != nil {
		m.listen.Store(true)
	}
}

func (m *serverMetrics) retry(reason string) {
	if m != nil {
		m.retries.With(reason).Inc()
	}
}

func (m *serverMetrics) waited(capability domain.CapabilityName, d time.Duration) {
	if m != nil {
		m.queueWait.With(string(capability)).Observe(max(d, 0).Seconds())
	}
}

// workloadFinished records a final status. previous is the record before
// the update, for the run time.
func (m *serverMetrics) workloadFinished(w domain.Workload, st domain.WorkloadStatus) {
	if m == nil {
		return
	}
	capability := string(w.EffectiveCapability())
	var outcome string
	switch st.State {
	case domain.WorkloadCompleted:
		outcome = "completed"
	case domain.WorkloadFailed:
		outcome = "failed"
	case domain.WorkloadCanceled:
		outcome = "canceled"
	default:
		return
	}
	m.finished.With(capability, outcome).Inc()
	if st.State == domain.WorkloadFailed && strings.HasPrefix(st.Error, "timed out after") {
		m.timeouts.With(capability).Inc()
	}
	if !st.StartedAt.IsZero() && !st.FinishedAt.IsZero() {
		m.duration.With(capability, outcome).Observe(max(st.FinishedAt.Sub(st.StartedAt), 0).Seconds())
	}
}

func (m *serverMetrics) jobFinished(state domain.JobState) {
	if m != nil {
		m.jobsDone.With(string(state)).Inc()
	}
}

func (m *serverMetrics) workflowFinished(state domain.JobState) {
	if m != nil {
		m.workflowsDone.With(string(state)).Inc()
	}
}

func (m *serverMetrics) spotChecked(state domain.SpotCheckState) {
	if m != nil {
		m.spotChecks.With(string(state)).Inc()
	}
}

func (m *serverMetrics) audited(log domain.AuditLog, kind string) {
	if m != nil {
		m.auditEvents.With(string(log), kind).Inc()
	}
}

func (m *serverMetrics) sseOpened() {
	if m != nil {
		m.sse.Add(1)
	}
}

func (m *serverMetrics) sseClosed() {
	if m != nil {
		m.sse.Add(-1)
	}
}

// ---- scrape-time collectors

// holdClass reduces an availability reason to a bounded class.
func holdClass(reason string) string {
	switch {
	case reason == "paused":
		return "paused"
	case strings.HasPrefix(reason, "outside its hours"):
		return "outside_hours"
	case strings.HasPrefix(reason, "on battery"):
		return "on_battery"
	case strings.HasPrefix(reason, "in use"):
		return "in_use"
	case strings.HasPrefix(reason, "hasn't reported"):
		return "no_report"
	}
	return "other"
}

var (
	allWorkloadStates = []domain.WorkloadState{domain.WorkloadQueued, domain.WorkloadPending, domain.WorkloadRunning,
		domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled, domain.WorkloadUnknown}
	allJobStates  = []domain.JobState{domain.JobRunning, domain.JobCompleted, domain.JobFailed, domain.JobCanceled}
	allNodeStates = []domain.NodeState{domain.NodeReady, domain.NodeOffline, domain.NodeConnected, domain.NodeDegraded}
	holdClasses   = []string{"paused", "outside_hours", "on_battery", "in_use", "no_report", "other"}
)

type nodeFacts struct {
	id    domain.NodeID
	state domain.NodeState
	age   float64
	slots int
	used  int
	self  *domain.AgentSelfMetrics
}

// fleetSnapshot is one pass over the registry, shared by the collectors and
// GET /status so both say the same thing.
type fleetSnapshot struct {
	byState map[domain.NodeState]int
	taking  int
	held    map[string]int
	nodes   []nodeFacts // sorted by ID
}

func (s *Server) fleetSnapshot(now time.Time) fleetSnapshot {
	fs := fleetSnapshot{byState: map[domain.NodeState]int{}, held: map[string]int{}}
	recs := s.Registry.List()
	for _, rec := range recs {
		id := rec.Node.Identity.NodeID
		fs.byState[rec.State]++
		if rec.State == domain.NodeReady {
			if ok, why := s.availableNow(rec, now); ok {
				fs.taking++
			} else {
				fs.held[holdClass(why)]++
			}
		}
		fs.nodes = append(fs.nodes, nodeFacts{id: id, state: rec.State, age: now.Sub(rec.LastSeen).Seconds(),
			slots: rec.slots(), used: s.Workloads.usageOn(id).running, self: rec.Self})
	}
	sort.Slice(fs.nodes, func(i, j int) bool { return fs.nodes[i].id < fs.nodes[j].id })
	return fs
}

func (m *serverMetrics) fleet(s *Server) fleetSnapshot {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if time.Since(m.fleetAt) > time.Second {
		m.fleetSnap, m.fleetAt = s.fleetSnapshot(time.Now()), time.Now()
	}
	return m.fleetSnap
}

func (m *serverMetrics) workloads(s *Server) []WorkloadRecord {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if time.Since(m.recsAt) > time.Second {
		m.recsSnap, m.recsAt = s.Workloads.List(), time.Now()
	}
	return m.recsSnap
}

func (m *serverMetrics) registerCollectors(s *Server) {
	r := m.reg
	type emitFn = func(float64, ...string)

	r.NewCollector("harness_build_info", "Build of this manager (value is always 1).", "gauge", []string{"version", "commit"},
		func(emit emitFn) { emit(1, Version, buildCommit()) })
	r.NewCollector("harness_manager_start_time_seconds", "When this manager process started (unix time).", "gauge", nil,
		func(emit emitFn) { emit(float64(m.start.Unix())) })
	r.NewCollector("harness_ready", "1 when /readyz would answer ready.", "gauge", nil,
		func(emit emitFn) {
			if ok, _ := s.ready(); ok {
				emit(1)
			} else {
				emit(0)
			}
		})

	// One fleet pass per scrape, shared by the node families below.
	r.NewCollector("harness_nodes", "Known nodes by state.", "gauge", []string{"state"}, func(emit emitFn) {
		snap := m.fleet(s)
		for _, st := range allNodeStates {
			emit(float64(snap.byState[st]), string(st))
		}
		for st, n := range snap.byState {
			if !containsState(allNodeStates, st) {
				emit(float64(n), string(st))
			}
		}
	})
	r.NewCollector("harness_nodes_taking_work", "Ready devices that accept new work right now.", "gauge", nil,
		func(emit emitFn) { emit(float64(m.fleet(s).taking)) })
	r.NewCollector("harness_nodes_held_back", "Ready devices not taking new work, by reason class.", "gauge", []string{"reason"},
		func(emit emitFn) {
			for _, c := range holdClasses {
				emit(float64(m.fleet(s).held[c]), c)
			}
		})
	r.NewCollector("harness_node_heartbeat_age_seconds_max", "Seconds since the quietest ready node was last heard from.", "gauge", nil,
		func(emit emitFn) {
			var worst float64
			for _, n := range m.fleet(s).nodes {
				if n.state == domain.NodeReady {
					worst = max(worst, n.age)
				}
			}
			emit(worst)
		})
	perNode := func(name, help, typ string, val func(nodeFacts) (float64, bool)) {
		r.NewCollector(name, help, typ, []string{"node"}, func(emit emitFn) {
			for i, n := range m.fleet(s).nodes {
				if i >= maxNodeSeries {
					break
				}
				if v, ok := val(n); ok {
					emit(v, string(n.id))
				}
			}
		})
	}
	perNode("harness_node_heartbeat_age_seconds", "Seconds since each node (first 50 by ID) was last heard from.", "gauge",
		func(n nodeFacts) (float64, bool) { return n.age, true })
	perNode("harness_node_slots_used", "Workload slots in use on each node.", "gauge",
		func(n nodeFacts) (float64, bool) { return float64(n.used), true })
	perNode("harness_node_slots", "Workload slots each node offers.", "gauge",
		func(n nodeFacts) (float64, bool) { return float64(n.slots), true })
	selfOf := func(f func(*domain.AgentSelfMetrics) int64) func(nodeFacts) (float64, bool) {
		return func(n nodeFacts) (float64, bool) {
			if n.self == nil {
				return 0, false
			}
			return float64(f(n.self)), true
		}
	}
	perNode("harness_agent_uptime_seconds", "Agent process uptime, as the agent reports it.", "gauge", selfOf(func(a *domain.AgentSelfMetrics) int64 { return a.UptimeSeconds }))
	perNode("harness_agent_workloads_run_total", "Workloads the agent finished since it started, as it reports it.", "counter", selfOf(func(a *domain.AgentSelfMetrics) int64 { return a.WorkloadsRun }))
	perNode("harness_agent_workloads_failed_total", "Of those, how many failed.", "counter", selfOf(func(a *domain.AgentSelfMetrics) int64 { return a.WorkloadsFailed }))
	perNode("harness_agent_cache_hits_total", "Input files the agent served from its cache.", "counter", selfOf(func(a *domain.AgentSelfMetrics) int64 { return a.CacheHits }))
	perNode("harness_agent_cache_misses_total", "Input files the agent had to download.", "counter", selfOf(func(a *domain.AgentSelfMetrics) int64 { return a.CacheMisses }))
	perNode("harness_agent_reconnects_total", "Times the agent reconnected to a manager since it started.", "counter", selfOf(func(a *domain.AgentSelfMetrics) int64 { return a.Reconnects }))

	// Workloads, the queue, jobs, workflows, plans, spot checks.
	r.NewCollector("harness_workloads", "Workloads the manager holds, by state.", "gauge", []string{"state"}, func(emit emitFn) {
		recs := m.workloads(s)
		counts := map[domain.WorkloadState]int{}
		for _, w := range recs {
			counts[w.Status.State]++
		}
		for _, st := range allWorkloadStates {
			emit(float64(counts[st]), string(st))
		}
	})
	r.NewCollector("harness_queue_depth", "Workloads waiting for a device with a free slot.", "gauge", nil, func(emit emitFn) {
		n := 0
		for _, w := range m.workloads(s) {
			if w.Status.State == domain.WorkloadQueued {
				n++
			}
		}
		emit(float64(n))
	})
	r.NewCollector("harness_queue_oldest_wait_seconds", "How long the oldest queued workload has waited.", "gauge", nil, func(emit emitFn) {
		var oldest float64
		now := time.Now()
		for _, w := range m.workloads(s) {
			if w.Status.State == domain.WorkloadQueued && !w.Status.QueuedAt.IsZero() {
				oldest = max(oldest, now.Sub(w.Status.QueuedAt).Seconds())
			}
		}
		emit(oldest)
	})
	r.NewCollector("harness_jobs", "Batch jobs by state.", "gauge", []string{"state"}, func(emit emitFn) {
		counts := map[domain.JobState]int{}
		for _, j := range s.jobs.list() {
			counts[j.State]++
		}
		for _, st := range allJobStates {
			emit(float64(counts[st]), string(st))
		}
	})
	r.NewCollector("harness_workflows", "Workflows by state.", "gauge", []string{"state"}, func(emit emitFn) {
		counts := map[domain.JobState]int{}
		for _, w := range s.workflows.list() {
			counts[w.State]++
		}
		for _, st := range allJobStates {
			emit(float64(counts[st]), string(st))
		}
	})
	r.NewCollector("harness_plans", "AI plans held in memory (the newest 100), by state.", "gauge", []string{"state"}, func(emit emitFn) {
		counts := map[PlanState]int{}
		for _, p := range s.plans.list() {
			counts[p.State]++
		}
		for _, st := range []PlanState{PlanPlanning, PlanProposed, PlanRefused, PlanFailed, PlanApproved, PlanRejected, PlanExpired} {
			emit(float64(counts[st]), string(st))
		}
	})
	r.NewCollector("harness_spot_checks_open", "Spot checks still being decided.", "gauge", nil, func(emit emitFn) {
		n := 0
		if t := s.spotTable(); t != nil {
			n = len(t.open())
		}
		emit(float64(n))
	})

	// Storage.
	r.NewCollector("harness_artifact_store_used_bytes", "Bytes of workload files stored.", "gauge", nil, func(emit emitFn) {
		if s.cfg.Artifacts != nil {
			used, _ := s.cfg.Artifacts.Usage()
			emit(float64(used))
		}
	})
	r.NewCollector("harness_artifact_store_limit_bytes", "Most bytes the artifact store may hold.", "gauge", nil, func(emit emitFn) {
		if s.cfg.Artifacts != nil {
			_, total := s.cfg.Artifacts.Usage()
			emit(float64(total))
		}
	})
	r.NewCollector("harness_store_size_bytes", "Size of the manager's database file.", "gauge", nil, func(emit emitFn) {
		if sz, ok := s.store.(interface{ SizeBytes() (int64, error) }); ok {
			if n, err := sz.SizeBytes(); err == nil {
				emit(float64(n))
			}
		}
	})

	// Failover and the event stream.
	r.NewCollector("harness_failover_term", "This manager's failover term (changes when a standby takes over).", "gauge", nil, func(emit emitFn) {
		s.failover.mu.Lock()
		defer s.failover.mu.Unlock()
		emit(float64(s.failover.term))
	})
	r.NewCollector("harness_failover_stepped_down", "1 when a newer manager superseded this one.", "gauge", nil, func(emit emitFn) {
		if s.isFenced() {
			emit(1)
		} else {
			emit(0)
		}
	})
	r.NewCollector("harness_standby_lag_seconds", "Seconds since the standby last copied this manager's state (absent with no standby or before its first copy).", "gauge", nil, func(emit emitFn) {
		f := s.failover
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.pair != nil && !f.lastSync.IsZero() {
			emit(time.Since(f.lastSync).Seconds())
		}
	})
	r.NewCollector("harness_standby_configured", "1 when this manager has a standby.", "gauge", nil, func(emit emitFn) {
		f := s.failover
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.pair != nil {
			emit(1)
		} else {
			emit(0)
		}
	})
	r.NewCollector("harness_event_stream_subscribers", "Open dashboard / client event streams (GET /events).", "gauge", nil,
		func(emit emitFn) { emit(float64(m.sse.Load())) })
}

func containsState(list []domain.NodeState, st domain.NodeState) bool {
	for _, x := range list {
		if x == st {
			return true
		}
	}
	return false
}

func (s *Server) isFenced() bool {
	s.failover.mu.Lock()
	defer s.failover.mu.Unlock()
	return s.failover.fenced
}

// ---- health

// ready reports whether this manager can do its job; the reason is a short
// fixed phrase (never a path or an error string).
func (s *Server) ready() (bool, string) {
	if s.isFenced() {
		return false, "stepped down"
	}
	if s.met != nil && !s.met.listen.Load() {
		return false, "not listening"
	}
	if p, ok := s.store.(interface{ Ping() error }); ok {
		if !s.storePing(p) {
			return false, "store not writable"
		}
	}
	return true, ""
}

// storePing caches the write probe for a few seconds: it commits to disk,
// and an orchestrator may poll every second.
func (s *Server) storePing(p interface{ Ping() error }) bool {
	now := time.Now().UnixNano()
	if at := s.pingAt.Load(); at != 0 && now-at < int64(5*time.Second) {
		return s.pingOK.Load()
	}
	ok := p.Ping() == nil
	s.pingOK.Store(ok)
	s.pingAt.Store(now)
	return ok
}

// livenessWindow is how stale the reconcile loop may be before /healthz
// says the event loop is stuck: well beyond any tick.
func (s *Server) livenessWindow() time.Duration {
	return 6*s.cfg.ReconcileInterval + 30*time.Second
}

func (s *Server) apiHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.met != nil {
		if last := s.met.lastTick.Load(); last != 0 && time.Since(time.Unix(0, last)) > s.livenessWindow() {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "stalled\n")
			return
		}
	}
	io.WriteString(w, "ok\n")
}

func (s *Server) apiReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if ok, why := s.ready(); !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "not ready: "+why+"\n")
		return
	}
	io.WriteString(w, "ready\n")
}

func (s *Server) apiMetrics(w http.ResponseWriter, r *http.Request) {
	if s.met == nil {
		http.Error(w, "metrics are not available", http.StatusNotFound)
		return
	}
	var buf bytes.Buffer
	if _, err := s.met.reg.WriteTo(&buf); err != nil {
		http.Error(w, "metrics failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// ---- request IDs and API instrumentation

type requestIDKey struct{}

// requestIDFrom is the ID of the API request ctx belongs to ("" outside one).
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// statusRecorder remembers the status and bytes written, and passes
// Flush through (the event stream needs it).
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

func statusClass(code int) string {
	if code < 100 || code > 599 {
		return "other"
	}
	return strconv.Itoa(code/100) + "xx"
}

// instrument wraps the operator API: a request ID (X-Request-Id, never
// taken from the client), then the request count and latency by route
// pattern, and the artifact bytes the operator moved.
func (s *Server) instrument(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		m := s.met
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		_, pattern := mux.Handler(r)
		route := pattern
		if route == "" {
			route = "unmatched" // never the raw path: that is attacker-chosen
		}
		var body *countingBody
		if pattern == "POST /artifacts" && r.Body != nil {
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		m.httpReqs.With(route, statusClass(rec.status)).Inc()
		if pattern != "GET /events" {
			m.httpDuration.With(route).Observe(time.Since(start).Seconds())
		}
		if body != nil {
			m.artifactIO.With("upload", "operator").Add(float64(body.n))
		}
		if pattern == "GET /artifacts/{sha}" && rec.status < 300 {
			m.artifactIO.With("download", "operator").Add(float64(rec.bytes))
		}
	})
}

// countAgentTransfers wraps the agents' file-transfer routes.
func (s *Server) countAgentTransfers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := s.met
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		var body *countingBody
		if r.Method == http.MethodPut && r.Body != nil {
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if body != nil {
			m.artifactIO.With("upload", "agent").Add(float64(body.n))
		} else if r.Method == http.MethodGet && rec.status < 300 {
			m.artifactIO.With("download", "agent").Add(float64(rec.bytes))
		}
	})
}

// observePath reports whether r asks for one of the three routes the
// read-only metrics token opens, and nothing else.
func observePath(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/metrics", "/healthz", "/readyz":
		return true
	}
	return false
}
