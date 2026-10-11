package agent

import (
	"sync/atomic"
	"time"

	"home-harness/internal/domain"
)

// selfCounters are the agent's own numbers (domain.AgentSelfMetrics), kept
// with atomics so the executor's goroutines, the receive loop and the
// heartbeat loop never share a lock over them.
type selfCounters struct {
	finished    atomic.Int64
	failed      atomic.Int64
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
	connections atomic.Int64
}

// noteStatus counts a terminal workload status. A busy refusal (the agent
// never ran it and the manager re-queues it) is not a run.
func (c *selfCounters) noteStatus(s domain.WorkloadStatus) {
	switch s.State {
	case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled:
	default:
		return
	}
	if s.State == domain.WorkloadFailed && s.Retryable && s.StartedAt.IsZero() {
		return
	}
	c.finished.Add(1)
	if s.State == domain.WorkloadFailed {
		c.failed.Add(1)
	}
}

func (c *selfCounters) noteInput(hit bool) {
	if hit {
		c.cacheHits.Add(1)
	} else {
		c.cacheMisses.Add(1)
	}
}

// noteConnected counts a registration with a manager.
func (c *selfCounters) noteConnected() { c.connections.Add(1) }

// report is what the heartbeat carries.
func (c *selfCounters) report(since time.Time) *domain.AgentSelfMetrics {
	return &domain.AgentSelfMetrics{
		UptimeSeconds:   int64(time.Since(since).Seconds()),
		WorkloadsRun:    c.finished.Load(),
		WorkloadsFailed: c.failed.Load(),
		CacheHits:       c.cacheHits.Load(),
		CacheMisses:     c.cacheMisses.Load(),
		Reconnects:      max(c.connections.Load()-1, 0),
	}
}
