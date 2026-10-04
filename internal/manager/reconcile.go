package manager

import (
	"context"
	"log"
	"time"

	"home-harness/internal/domain"
)

// defaultReconcileInterval is used when Config.ReconcileInterval is unset
// (zero), so every existing caller/test that constructs a Config without
// it (as they do for every other new tunable added over this project's
// life) gets a sane default instead of a zero-duration ticker panic.
const defaultReconcileInterval = 5 * time.Second

// restartBackoffBase/Max bound the delay between restart attempts for a
// genuinely failing workload, doubling per attempt like agent.nextBackoff
// but recomputed fresh from persisted Restart.Count each tick rather than
// carried forward across an open session — a restart count survives a
// manager restart, a connection's backoff does not, so the two aren't
// shared code despite the similar shape.
const (
	restartBackoffBase = 5 * time.Second
	restartBackoffMax  = 5 * time.Minute
)

// restartHealthyRunThreshold is how long an attempt must have been observed
// running before its eventual failure counts as "healthy" and resets
// Restart.BackoffCount (see MarkRestarting) rather than feeding the
// doubling curve. Kept distinct from restartBackoffMax even though they
// currently share a value: one is a delay bound, the other a health bar,
// and a future tune of one shouldn't silently move the other.
const restartHealthyRunThreshold = 5 * time.Minute

// reconcileWorkloads periodically restarts workloads whose RestartPolicy
// wants them running again. A separate ticker from monitorHeartbeats,
// deliberately: monitorHeartbeats produces the FAILED/OFFLINE states this
// loop consumes (via failWorkloadsFor), and sharing one tick would raise
// an ordering question with no clean answer — restarted the same tick a
// node is marked offline, before it could possibly have reconnected? A
// separate loop makes "failed this tick, considered for restart next
// tick" unambiguous.
func (s *Server) reconcileWorkloads(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	gc := time.NewTicker(time.Hour)
	defer gc.Stop()
	expire := time.NewTicker(expireEvery)
	defer expire.Stop()
	s.expireWorkloads()
	s.collectArtifacts()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gc.C:
			s.collectArtifacts()
		case <-expire.C:
			s.expireWorkloads()
		case <-ticker.C:
			s.keepAwakeTick(time.Now())
			// The queue first: a slot that just freed goes to waiting work
			// in priority order, before a restart or a job's next attempt
			// (each placed at once when there is room) could take it.
			s.dispatchQueued(ctx)
			s.reconcileOnce(ctx)
			s.advanceJobs(ctx)
			s.dispatchQueued(ctx) // also catches requeue backoffs expiring
			s.sweepGrants()
		case <-s.dispatchKick:
			s.dispatchQueued(ctx)
			s.advanceJobs(ctx)
			s.dispatchQueued(ctx)
		}
	}
}

// reconcileOnce is the per-tick body, factored out for direct unit testing
// without real timers — mirrors why nextBackoff (internal/agent/agent.go)
// is a separately-tested pure function.
func (s *Server) reconcileOnce(ctx context.Context) {
	for _, rec := range s.Workloads.RestartCandidates(time.Now()) {
		s.restartWorkload(ctx, rec)
	}
}

// restartWorkload attempts to re-run a restart-eligible workload.
func (s *Server) restartWorkload(ctx context.Context, rec WorkloadRecord) {
	// A FAILED status with a zero StartedAt means this attempt was
	// rejected before any process ever launched (e.g. an insecure-mode
	// refusal; a busy refusal never lands here — it is re-queued, see
	// handleBusyRefusal), not a real crash. Deferring here instead of
	// counting it keeps a placement-time rejection from burning a
	// restart-count increment and a backoff doubling for a workload that
	// never actually failed. A restart that finds every eligible node full
	// is deferred the same way and retried next tick.
	if rec.Status.State == domain.WorkloadFailed && rec.Status.StartedAt.IsZero() {
		s.Workloads.DeferRestart(rec.Workload.ID, time.Now().Add(s.cfg.ReconcileInterval))
		return
	}

	// Policy may have changed since it was submitted: a type that is now
	// disabled is not brought back (CANCELED is never restarted).
	if err := s.checkPolicy(rec.Workload.EffectiveCapability()); err != nil {
		if blocked, ok := s.Workloads.blockRestart(rec.Workload.ID, err.Error()); ok {
			s.persistWorkloadRecord(blocked)
			log.Printf("workload.canceled: %s (restart %v)", blocked.Workload.ID, err)
			s.publish(domain.EventWorkloadCanceled, blocked.Workload.Target, map[string]any{"workloadId": string(blocked.Workload.ID), "reason": "blocked by policy"})
		}
		return
	}

	// A Pinned workload stays on its exact node across restarts (v2's
	// "explicit target is never silently overridden" precedent); an
	// originally-auto-placed one is free to land anywhere currently
	// eligible — the "self-healing across the fleet" case.
	var restartTarget domain.NodeID
	if rec.Workload.Pinned {
		restartTarget = rec.Workload.Target
	}
	s.placeMu.Lock()
	p := placementFor(rec.Workload)
	p.target = restartTarget
	targetRec, resolvedTarget, err := s.resolve(p, nil)
	if err != nil {
		s.placeMu.Unlock()
		// No eligible node right now (e.g. right after a manager restart,
		// before any node has reconnected — Registry.Seed starts nodes
		// OFFLINE — or a Pinned node that's temporarily down). Not the
		// workload's fault: defer without counting it.
		s.Workloads.DeferRestart(rec.Workload.ID, time.Now().Add(s.cfg.ReconcileInterval))
		return
	}

	newRec, ok := s.Workloads.MarkRestarting(rec.Workload.ID, resolvedTarget)
	s.placeMu.Unlock()
	if !ok {
		return // vanished between the RestartCandidates snapshot and now
	}
	s.Workloads.DeferRestart(newRec.Workload.ID, time.Now().Add(backoffFor(newRec.Restart.BackoffCount, restartBackoffBase, restartBackoffMax)))
	newRec, _ = s.Workloads.Get(newRec.Workload.ID) // re-fetch to persist the NextRestartAt just set
	s.persistWorkloadRecord(newRec)

	s.assign(ctx, targetRec.Conn, newRec.Workload)
	log.Printf("workload.restarted: %s on %s (attempt %d)", newRec.Workload.ID, resolvedTarget, newRec.Restart.Count)
	s.publish(domain.EventWorkloadAssigned, resolvedTarget, map[string]any{
		"workloadId":   string(newRec.Workload.ID),
		"command":      newRec.Workload.Command,
		"capability":   string(newRec.Workload.EffectiveCapability()),
		"restartCount": newRec.Restart.Count,
	})
}

// backoffFor doubles per restart attempt, capped at max — indexed on
// Restart.BackoffCount (not the lifetime Restart.Count) and stateless
// (recomputed each call) rather than carrying a running duration forward,
// since BackoffCount is what actually persists across a manager restart.
// The "|| d <= 0" guard is defensive: time.Duration is an int64 count of
// nanoseconds, so repeated doubling would eventually overflow to negative
// if restartCount ever got large enough to reach it — which in turn would
// make NextRestartAt land in the past and turn the backoff into a hot
// loop. BackoffCount resets on a healthy run, so this is normally
// unreachable, but the guard costs nothing and removes the failure mode
// entirely rather than relying on that always being true.
func backoffFor(restartCount int, base, max time.Duration) time.Duration {
	d := base
	for i := 0; i < restartCount; i++ {
		d *= 2
		if d >= max || d <= 0 {
			return max
		}
	}
	return d
}
