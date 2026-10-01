package manager

import (
	"sync"
	"time"

	"home-harness/internal/domain"
)

// WorkloadRecord is the manager's view of one workload submission: the
// original request, its most recently reported status, and (if it has a
// non-Never RestartPolicy) its restart bookkeeping.
type WorkloadRecord struct {
	Workload domain.Workload
	Status   domain.WorkloadStatus
	Restart  domain.RestartState
	// cancelRequested: the operator canceled this attempt while it was
	// PENDING, so a busy refusal racing that cancel must not re-queue it
	// (queue.go). In memory only; it matters for one assignment's lifetime.
	cancelRequested bool
}

// WorkloadRegistry tracks every workload the manager has submitted, keyed
// by WorkloadID. It is structurally separate from Registry (nodes) for the
// same reason runtime and persistent node state are split (v1.md §13): a
// workload is long-lived — it has a RUNNING phase between assignment and
// completion — unlike a Command's single request/reply exchange.
type WorkloadRegistry struct {
	mu        sync.RWMutex
	workloads map[domain.WorkloadID]*WorkloadRecord
	// lastQueuedAt keeps queue timestamps strictly increasing (queue.go):
	// back-to-back submissions can read the same wall-clock time.
	lastQueuedAt time.Time
}

// NewWorkloadRegistry returns an empty workload registry.
func NewWorkloadRegistry() *WorkloadRegistry {
	return &WorkloadRegistry{workloads: make(map[domain.WorkloadID]*WorkloadRecord)}
}

// Put inserts a newly submitted workload with its initial status and a
// zero-value RestartState (a fresh submission has no restart history yet,
// distinct from MarkRestarting's job of incrementing it).
func (wr *WorkloadRegistry) Put(w domain.Workload, status domain.WorkloadStatus) WorkloadRecord {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	rec := &WorkloadRecord{Workload: w, Status: status}
	wr.workloads[w.ID] = rec
	return *rec
}

// UpdateStatus records a newly reported status for a known workload,
// returning the resulting record (so the caller can verify the report
// actually came from that workload's target node, and persist the full
// record including its untouched Restart state) and whether the workload
// was known at all.
func (wr *WorkloadRegistry) UpdateStatus(status domain.WorkloadStatus) (WorkloadRecord, bool) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	rec, ok := wr.workloads[status.ID]
	if !ok {
		return WorkloadRecord{}, false
	}
	rec.Status = status
	return *rec, true
}

// Get returns the record for a workload ID, if known.
func (wr *WorkloadRegistry) Get(id domain.WorkloadID) (WorkloadRecord, bool) {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	rec, ok := wr.workloads[id]
	if !ok {
		return WorkloadRecord{}, false
	}
	return *rec, true
}

// List returns a snapshot of every known workload.
func (wr *WorkloadRegistry) List() []WorkloadRecord {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	out := make([]WorkloadRecord, 0, len(wr.workloads))
	for _, rec := range wr.workloads {
		out = append(out, *rec)
	}
	return out
}

// FailInFlightFor marks every PENDING/RUNNING workload targeting nodeID as
// FAILED, returning the resulting records. This is the workload analogue
// of Server.failPendingCommandsFor: a node going offline mid-workload
// (churn is normal — baseline §9 rule 4) must not leave that workload
// stuck RUNNING forever with no possible future update.
func (wr *WorkloadRegistry) FailInFlightFor(nodeID domain.NodeID, reason string) []WorkloadRecord {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	var changed []WorkloadRecord
	for _, rec := range wr.workloads {
		if rec.Workload.Target != nodeID {
			continue
		}
		if rec.Status.State != domain.WorkloadPending && rec.Status.State != domain.WorkloadRunning {
			continue
		}
		rec.Status.State = domain.WorkloadFailed
		rec.Status.Error = reason
		rec.Status.FinishedAt = time.Now().UTC()
		rec.Status.NodeLost = true
		changed = append(changed, *rec)
	}
	return changed
}

// CancelPinnedTo marks every workload pinned to nodeID that is still in
// flight, or that its restart policy would bring back, as CANCELED, and
// returns the changed records. It exists for revoked nodes: a pinned
// workload may only ever run on its node, so without this the reconciler
// would defer its restart forever against a node that can never return.
// CANCELED is terminal and never restarted. Unpinned workloads are left to
// FailInFlightFor, since re-placing them elsewhere is still legitimate.
func (wr *WorkloadRegistry) CancelPinnedTo(nodeID domain.NodeID, reason string) []WorkloadRecord {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	var changed []WorkloadRecord
	now := time.Now().UTC()
	for _, rec := range wr.workloads {
		if !rec.Workload.Pinned || rec.Workload.Target != nodeID {
			continue
		}
		state := rec.Status.State
		inFlight := state == domain.WorkloadPending || state == domain.WorkloadRunning || state == domain.WorkloadQueued
		if !inFlight && !rec.Workload.RestartPolicy.WantsRestartAfter(state) {
			continue
		}
		rec.Status.State = domain.WorkloadCanceled
		rec.Status.Error = reason
		if inFlight || rec.Status.FinishedAt.IsZero() {
			rec.Status.FinishedAt = now
		}
		changed = append(changed, *rec)
	}
	return changed
}

// RestartCandidates returns copies of every workload whose policy wants a
// restart from its current state (RestartPolicy.WantsRestartAfter) and
// whose NextRestartAt has passed. Returns copies and takes no action (like
// ExpireStale) so the caller can re-place candidates without holding the
// registry lock across a network send.
func (wr *WorkloadRegistry) RestartCandidates(now time.Time) []WorkloadRecord {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	var out []WorkloadRecord
	for _, rec := range wr.workloads {
		if !rec.Workload.RestartPolicy.WantsRestartAfter(rec.Status.State) {
			continue
		}
		if now.Before(rec.Restart.NextRestartAt) {
			continue
		}
		out = append(out, *rec)
	}
	return out
}

// MarkRestarting resets id's status to PENDING on a new target and
// increments Restart.Count. It explicitly clears the previous attempt's
// Stdout/Stderr/ExitCode/Error/StartedAt/FinishedAt — without this,
// GET /workloads/{id} would show attempt N-1's output sitting next to
// attempt N's PENDING state. Distinct from Put (a brand-new submission,
// no restart count carried forward) and UpdateStatus (agent-reported,
// never touches Restart).
//
// Restart.Count is a lifetime total (for display) and always increments.
// Restart.BackoffCount drives backoffFor and is reset to 0 instead when the
// just-ended attempt's own StartedAt/FinishedAt (still in place on
// rec.Status at the top of this function, before they're overwritten below)
// show it stayed running for at least restartHealthyRunThreshold. Without
// this reset, a service that ran fine for hours before one crash would
// inherit whatever multi-minute wait its full lifetime Count implies,
// instead of a short first-failure backoff.
//
// FinishedAt is only ever non-zero for a genuine terminal report from the
// agent (FAILED after actually running) — the UNKNOWN/orphaned-by-a-
// manager-restart path (Seed) never gets one, since nothing ever resolved
// its outcome. There's no reliable duration signal in that case, so it
// always takes the increment branch rather than guessing.
func (wr *WorkloadRegistry) MarkRestarting(id domain.WorkloadID, target domain.NodeID) (WorkloadRecord, bool) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	rec, ok := wr.workloads[id]
	if !ok {
		return WorkloadRecord{}, false
	}
	if !rec.Status.StartedAt.IsZero() && !rec.Status.FinishedAt.IsZero() &&
		rec.Status.FinishedAt.Sub(rec.Status.StartedAt) >= restartHealthyRunThreshold {
		rec.Restart.BackoffCount = 0
	} else {
		rec.Restart.BackoffCount++
	}
	rec.Workload.Target = target
	rec.Status = domain.WorkloadStatus{ID: id, Target: target, State: domain.WorkloadPending}
	rec.cancelRequested = false
	rec.Restart.Count++
	return *rec, true
}

// DeferRestart pushes NextRestartAt without incrementing Restart.Count —
// for a workload that couldn't be restarted this tick through no fault of
// its own (no eligible node yet, or a placement-time rejection rather than
// a real execution failure). Neither case is a genuine crash, so neither
// should feed the exponential backoff curve MarkRestarting's callers use.
func (wr *WorkloadRegistry) DeferRestart(id domain.WorkloadID, at time.Time) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if rec, ok := wr.workloads[id]; ok {
		rec.Restart.NextRestartAt = at
	}
}

// Seed restores a workload from persistence after a manager restart. A
// workload that was PENDING or RUNNING when last persisted has its true
// outcome now unknown — the agent may have finished it, failed it, or
// still be running it — so it is seeded as UNKNOWN rather than kept
// RUNNING forever or silently dropped. If it has a non-Never RestartPolicy,
// the reconciliation loop (internal/manager/reconcile.go) picks this up
// from UNKNOWN on its own; this is what actually resolves the gap v1 left
// open ("v1 does not attempt reconciliation... that is v3 scope").
func (wr *WorkloadRegistry) Seed(pw domain.PersistedWorkload) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	status := pw.Status
	if status.State == domain.WorkloadPending || status.State == domain.WorkloadRunning {
		status.State = domain.WorkloadUnknown
	}
	if status.QueuedAt.After(wr.lastQueuedAt) {
		wr.lastQueuedAt = status.QueuedAt
	}
	wr.workloads[pw.Workload.ID] = &WorkloadRecord{Workload: pw.Workload, Status: status, Restart: pw.Restart}
}
