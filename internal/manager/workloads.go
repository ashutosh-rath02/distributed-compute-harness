package manager

import (
	"sync"
	"time"

	"home-harness/internal/domain"
)

// WorkloadRecord is the manager's view of one workload submission: the
// original request plus its most recently reported status.
type WorkloadRecord struct {
	Workload domain.Workload
	Status   domain.WorkloadStatus
}

// WorkloadRegistry tracks every workload the manager has submitted, keyed
// by WorkloadID. It is structurally separate from Registry (nodes) for the
// same reason runtime and persistent node state are split (v1.md §13): a
// workload is long-lived — it has a RUNNING phase between assignment and
// completion — unlike a Command's single request/reply exchange.
type WorkloadRegistry struct {
	mu        sync.RWMutex
	workloads map[domain.WorkloadID]*WorkloadRecord
}

// NewWorkloadRegistry returns an empty workload registry.
func NewWorkloadRegistry() *WorkloadRegistry {
	return &WorkloadRegistry{workloads: make(map[domain.WorkloadID]*WorkloadRecord)}
}

// Put inserts a newly submitted workload with its initial status.
func (wr *WorkloadRegistry) Put(w domain.Workload, status domain.WorkloadStatus) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	wr.workloads[w.ID] = &WorkloadRecord{Workload: w, Status: status}
}

// UpdateStatus records a newly reported status for a known workload,
// returning the workload it belongs to (so the caller can verify the
// report actually came from that workload's target node) and whether the
// workload was known at all.
func (wr *WorkloadRegistry) UpdateStatus(status domain.WorkloadStatus) (domain.Workload, bool) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	rec, ok := wr.workloads[status.ID]
	if !ok {
		return domain.Workload{}, false
	}
	rec.Status = status
	return rec.Workload, true
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
// FAILED, returning the IDs actually changed. This is the workload
// analogue of Server.failPendingCommandsFor: a node going offline
// mid-workload (churn is normal — baseline §9 rule 4) must not leave that
// workload stuck RUNNING forever with no possible future update.
func (wr *WorkloadRegistry) FailInFlightFor(nodeID domain.NodeID, reason string) []domain.WorkloadID {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	var changed []domain.WorkloadID
	for id, rec := range wr.workloads {
		if rec.Workload.Target != nodeID {
			continue
		}
		if rec.Status.State != domain.WorkloadPending && rec.Status.State != domain.WorkloadRunning {
			continue
		}
		rec.Status.State = domain.WorkloadFailed
		rec.Status.Error = reason
		rec.Status.FinishedAt = time.Now().UTC()
		changed = append(changed, id)
	}
	return changed
}

// Seed restores a workload from persistence after a manager restart. A
// workload that was PENDING or RUNNING when last persisted has its true
// outcome now unknown — the agent may have finished it, failed it, or
// still be running it, and v1 does not reconcile with the agent's actual
// state (that is v3 scope) — so it is seeded as UNKNOWN rather than kept
// RUNNING forever or silently dropped.
func (wr *WorkloadRegistry) Seed(pw domain.PersistedWorkload) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	status := pw.Status
	if status.State == domain.WorkloadPending || status.State == domain.WorkloadRunning {
		status.State = domain.WorkloadUnknown
	}
	wr.workloads[pw.Workload.ID] = &WorkloadRecord{Workload: pw.Workload, Status: status}
}
