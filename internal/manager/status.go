package manager

import (
	"net/http"
	"sort"
	"time"

	"home-harness/internal/domain"
)

// GET /status: one summary of the whole manager, for `harnessctl status` and
// `top` and the dashboard's Health card. Operator credentials only (the
// metrics token does not open it). It holds no secrets; the only free text
// in it is a device name and a failure reason, both clipped, and the
// dashboard escapes them.

type statusNode struct {
	ID                  domain.NodeID            `json:"id"`
	Name                string                   `json:"name"`
	State               domain.NodeState         `json:"state"`
	AgentVersion        string                   `json:"agentVersion"`
	Slots               int                      `json:"slots"`
	Running             int                      `json:"running"`
	HeartbeatAgeSeconds float64                  `json:"heartbeatAgeSeconds"`
	TakingWork          bool                     `json:"takingWork"`
	HeldBack            string                   `json:"heldBack,omitempty"`
	Self                *domain.AgentSelfMetrics `json:"self,omitempty"`
}

type statusFailure struct {
	Workload   domain.WorkloadID `json:"workload"`
	Capability string            `json:"capability"`
	Node       domain.NodeID     `json:"node"`
	Error      string            `json:"error"`
	At         time.Time         `json:"at"`
}

type statusView struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	Ready         bool   `json:"ready"`
	NotReady      string `json:"notReady,omitempty"`

	Nodes struct {
		Total      int            `json:"total"`
		ByState    map[string]int `json:"byState"`
		TakingWork int            `json:"takingWork"`
		HeldBack   map[string]int `json:"heldBack"`
		List       []statusNode   `json:"list"`
	} `json:"nodes"`
	Queue struct {
		Depth             int     `json:"depth"`
		OldestWaitSeconds float64 `json:"oldestWaitSeconds"`
	} `json:"queue"`
	Workloads      map[string]int  `json:"workloads"`
	Jobs           map[string]int  `json:"jobs"`
	Workflows      map[string]int  `json:"workflows"`
	RecentFailures []statusFailure `json:"recentFailures"`
	Standby        struct {
		Configured  bool    `json:"configured"`
		Term        uint64  `json:"term"`
		SteppedDown bool    `json:"steppedDown"`
		LagSeconds  float64 `json:"lagSeconds"` // -1: not copied yet
	} `json:"standby"`
	Disk struct {
		ArtifactUsedBytes  int64 `json:"artifactUsedBytes"`
		ArtifactLimitBytes int64 `json:"artifactLimitBytes"`
		StoreBytes         int64 `json:"storeBytes"`
	} `json:"disk"`
}

const recentFailuresShown = 8

func (s *Server) statusSummary() statusView {
	now := time.Now()
	var v statusView
	v.Version, v.Commit = Version, buildCommit()
	if s.met != nil {
		v.UptimeSeconds = int64(now.Sub(s.met.start).Seconds())
	}
	ok, why := s.ready()
	v.Ready, v.NotReady = ok, why

	fs := s.fleetSnapshot(now)
	v.Nodes.Total = len(fs.nodes)
	v.Nodes.ByState = map[string]int{}
	for st, n := range fs.byState {
		v.Nodes.ByState[string(st)] = n
	}
	v.Nodes.TakingWork, v.Nodes.HeldBack = fs.taking, fs.held
	v.Nodes.List = []statusNode{}
	for i, n := range fs.nodes {
		if i >= 100 {
			break
		}
		sn := statusNode{ID: n.id, State: n.state, Slots: n.slots, Running: n.used, HeartbeatAgeSeconds: n.age, Self: n.self}
		if rec, ok := s.Registry.Get(n.id); ok {
			sn.Name = clip(s.nodeDisplayName(n.id))
			sn.AgentVersion = clip(rec.Node.AgentVersion)
			if n.state == domain.NodeReady {
				if up, why := s.availableNow(rec, now); up {
					sn.TakingWork = true
				} else {
					sn.HeldBack = holdClass(why)
				}
			}
		}
		v.Nodes.List = append(v.Nodes.List, sn)
	}

	v.Workloads = map[string]int{}
	var failed []WorkloadRecord
	for _, w := range s.Workloads.List() {
		v.Workloads[string(w.Status.State)]++
		switch w.Status.State {
		case domain.WorkloadQueued:
			v.Queue.Depth++
			if !w.Status.QueuedAt.IsZero() {
				v.Queue.OldestWaitSeconds = max(v.Queue.OldestWaitSeconds, now.Sub(w.Status.QueuedAt).Seconds())
			}
		case domain.WorkloadFailed:
			failed = append(failed, w)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Status.FinishedAt.After(failed[j].Status.FinishedAt) })
	v.RecentFailures = []statusFailure{}
	for _, w := range failed[:min(len(failed), recentFailuresShown)] {
		v.RecentFailures = append(v.RecentFailures, statusFailure{Workload: w.Workload.ID, Capability: string(w.Workload.EffectiveCapability()),
			Node: w.Workload.Target, Error: clip(w.Status.Error), At: w.Status.FinishedAt})
	}
	v.Jobs, v.Workflows = map[string]int{}, map[string]int{}
	for _, j := range s.jobs.list() {
		v.Jobs[string(j.State)]++
	}
	for _, wf := range s.workflows.list() {
		v.Workflows[string(wf.State)]++
	}

	f := s.failover
	f.mu.Lock()
	v.Standby.Term, v.Standby.SteppedDown, v.Standby.Configured = f.term, f.fenced, f.pair != nil
	v.Standby.LagSeconds = -1
	if f.pair != nil && !f.lastSync.IsZero() {
		v.Standby.LagSeconds = time.Since(f.lastSync).Seconds()
	}
	f.mu.Unlock()

	if s.cfg.Artifacts != nil {
		v.Disk.ArtifactUsedBytes, v.Disk.ArtifactLimitBytes = s.cfg.Artifacts.Usage()
	}
	if sz, ok := s.store.(interface{ SizeBytes() (int64, error) }); ok {
		v.Disk.StoreBytes, _ = sz.SizeBytes()
	}
	return v
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusSummary())
}

// registerObserveRoutes adds the observability routes to the operator API.
func (s *Server) registerObserveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /metrics", s.apiMetrics)
	mux.HandleFunc("GET /healthz", s.apiHealthz)
	mux.HandleFunc("GET /readyz", s.apiReadyz)
	mux.HandleFunc("GET /status", s.apiStatus)
}

// registerStandbyProbes is what a standby answers: alive, and not ready
// (it serves no devices until promoted).
func registerStandbyProbes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("not ready: standby\n"))
	})
}

// setSelf records an agent's self-reported counters, but only from a node
// that advertised domain.FeatureSelfMetrics (and bounded: no negatives).
func (r *Registry) setSelf(id domain.NodeID, m *domain.AgentSelfMetrics) {
	if m == nil {
		return
	}
	clean := m.Clean()
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.nodes[id]; ok && rec.hasAgentFeature(domain.FeatureSelfMetrics) {
		rec.Self = &clean
	}
}

// admissionKind is how a node was admitted, without the code or token
// prefix that follows the colon.
func admissionKind(by string) string {
	for i := 0; i < len(by); i++ {
		if by[i] == ':' {
			return by[:i]
		}
	}
	return by
}
