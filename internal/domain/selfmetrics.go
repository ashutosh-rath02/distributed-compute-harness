package domain

// FeatureSelfMetrics means the agent reports a few counters about itself in
// every heartbeat (AgentSelfMetrics), so the manager can show them in
// GET /nodes/{id} and /metrics. Agents without it send nothing and the
// manager shows nothing for them; nothing else changes.
const FeatureSelfMetrics = "self-metrics.v1"

// AgentSelfMetrics is the whole of what an agent says about itself: five
// numbers, no text, so it is bounded by construction. Like everything an
// agent reports it is an untrusted hint, for display and monitoring only:
// no placement or security decision reads it.
//
// The counts are since the agent process started (they reset on a restart,
// which a Prometheus counter handles as an ordinary reset).
type AgentSelfMetrics struct {
	UptimeSeconds   int64 `json:"uptimeSeconds"`
	WorkloadsRun    int64 `json:"workloadsRun"`    // finished with any outcome
	WorkloadsFailed int64 `json:"workloadsFailed"` // of those, FAILED
	CacheHits       int64 `json:"cacheHits"`       // input files served from the local cache
	CacheMisses     int64 `json:"cacheMisses"`     // input files downloaded instead
	Reconnects      int64 `json:"reconnects"`      // connections to a manager after the first
}

// CacheHitRate is hits / (hits + misses), or -1 with no lookups yet.
func (m AgentSelfMetrics) CacheHitRate() float64 {
	if total := m.CacheHits + m.CacheMisses; total > 0 {
		return float64(m.CacheHits) / float64(total)
	}
	return -1
}

// Clean bounds an untrusted report: no negative counts.
func (m AgentSelfMetrics) Clean() AgentSelfMetrics {
	for _, p := range []*int64{&m.UptimeSeconds, &m.WorkloadsRun, &m.WorkloadsFailed, &m.CacheHits, &m.CacheMisses, &m.Reconnects} {
		if *p < 0 {
			*p = 0
		}
	}
	return m
}
