package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Capacity reservation and the work queue.
//
// A node runs up to its advertised slot count of workloads at once
// (domain.Manifest.WorkloadSlots; absent = 1, exactly how agents that
// predate slots behave). Reservations are never kept as counters that
// could drift: a node's usage is *derived* from the workload registry
// every time — every PENDING/RUNNING workload targeting it — so every exit
// path (completion, cancel, node loss, revocation, restart, a manager
// restart turning in-flight work UNKNOWN) releases capacity automatically.
//
// A submission that some ready node could run, but none has room for right
// now, is QUEUED rather than rejected, and dispatched oldest-first as
// capacity frees. Submissions no ready node could ever satisfy are still
// rejected, as is submitting with no ready node at all — queueing those
// would only park work in the void.
//
// Same-machine identities (fleet.go) are deliberately NOT pooled here: the
// host fingerprint is an untrusted hint, so each identity keeps its own
// slots. The operator sees the same-machine warning and decides.

// errNoRoom means every eligible node is full right now: queue it.
var errNoRoom = errors.New("manager: no eligible node has free capacity right now")

// requeueBackoff bounds how soon a workload refused by a busy agent (a
// race with the manager's view of its slots) is tried again. busyHoldOff is
// how long the refusing node gets no new work unless it reports a finished
// workload first. It is a timeout, not "until it finishes something": the
// refusal can cross that node's own completion report on the wire, and a
// node with nothing left running would then never be tried again.
const (
	requeueBackoffBase = 1 * time.Second
	requeueBackoffMax  = 30 * time.Second
	busyHoldOff        = 5 * time.Second
)

// nodeUsage is what a node's in-flight workloads have reserved.
type nodeUsage struct {
	running int
	cores   float64
	memory  uint64
	// perCapability counts running workloads by capability, for catalog
	// types limited per node (catalog.Type.MaxPerNode).
	perCapability map[domain.CapabilityName]int
}

// usageOn derives node's current reservations from its in-flight work.
func (wr *WorkloadRegistry) usageOn(node domain.NodeID) nodeUsage {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	var u nodeUsage
	for _, rec := range wr.workloads {
		if rec.Workload.Target != node {
			continue
		}
		if rec.Status.State != domain.WorkloadPending && rec.Status.State != domain.WorkloadRunning {
			continue
		}
		u.add(rec.Workload)
	}
	return u
}

// usageByNode is usageOn for every node at once, in one scan — a dispatch
// pass's snapshot.
func (wr *WorkloadRegistry) usageByNode() map[domain.NodeID]nodeUsage {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	out := make(map[domain.NodeID]nodeUsage)
	for _, rec := range wr.workloads {
		if rec.Status.State != domain.WorkloadPending && rec.Status.State != domain.WorkloadRunning {
			continue
		}
		u := out[rec.Workload.Target]
		u.add(rec.Workload)
		out[rec.Workload.Target] = u
	}
	return out
}

func (u *nodeUsage) add(w domain.Workload) {
	u.running++
	u.cores += w.Requirements.MinCPUCores
	u.memory += w.Requirements.MinMemoryBytes
	if u.perCapability == nil {
		u.perCapability = map[domain.CapabilityName]int{}
	}
	u.perCapability[w.EffectiveCapability()]++
}

// perNodeRoom reports whether u leaves room for one more of capability
// under its catalog type's MaxPerNode.
func perNodeRoom(u nodeUsage, capability domain.CapabilityName) (bool, string) {
	t, ok := catalog.Lookup(capability)
	if !ok || t.MaxPerNode <= 0 || u.perCapability[capability] < t.MaxPerNode {
		return true, ""
	}
	return false, fmt.Sprintf("already runs %d %s (at most %d per node)", u.perCapability[capability], capability, t.MaxPerNode)
}

// hasRoom reports whether rec can take one more workload with req on top
// of what's already reserved, and if not, why. It complements nodeFits
// (which asks whether the node could run it at all, idle).
func hasRoom(rec *NodeRecord, u nodeUsage, req domain.ResourceRequirements) (bool, string) {
	if time.Now().Before(rec.busyUntil) {
		return false, "just refused work for lack of a free slot; holding off briefly"
	}
	if u.running >= rec.slots() {
		return false, fmt.Sprintf("all %d workload slots in use", rec.slots())
	}
	if req.MinCPUCores > 0 {
		cores, _ := domain.StaticCapacity(rec.Resources, domain.ResourceCPUCores)
		if cores-u.cores < req.MinCPUCores {
			return false, fmt.Sprintf("%.2f of %.2f cores already reserved", u.cores, cores)
		}
	}
	// Conservative: the live figure may already reflect running work that
	// is also reserved, so this can under-place, never over-place.
	if req.MinMemoryBytes > 0 && rec.LastMetrics.MemoryAvailableBytes < u.memory+req.MinMemoryBytes {
		return false, fmt.Sprintf("%d bytes free with %d reserved", rec.LastMetrics.MemoryAvailableBytes, u.memory)
	}
	return true, ""
}

// enqueue stores a new submission as QUEUED. Its QueuedAt is strictly
// later than any before it — two submissions in the same clock tick would
// otherwise tie and be ordered by random ID, not arrival.
func (wr *WorkloadRegistry) enqueue(w domain.Workload, now time.Time) WorkloadRecord {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	queuedAt := wr.nextQueuedAtLocked(now)
	rec := &WorkloadRecord{Workload: w, Status: domain.WorkloadStatus{ID: w.ID, Target: w.Target, State: domain.WorkloadQueued, QueuedAt: queuedAt}}
	wr.workloads[w.ID] = rec
	return *rec
}

func (wr *WorkloadRegistry) nextQueuedAtLocked(now time.Time) time.Time {
	if !now.After(wr.lastQueuedAt) {
		now = wr.lastQueuedAt.Add(time.Nanosecond)
	}
	wr.lastQueuedAt = now
	return now
}

// queued returns QUEUED workloads ready for a placement attempt, oldest
// first.
func (wr *WorkloadRegistry) queued(now time.Time) []WorkloadRecord {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	var out []WorkloadRecord
	for _, rec := range wr.workloads {
		if rec.Status.State == domain.WorkloadQueued && !now.Before(rec.Status.NotBefore) {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Status.QueuedAt.Equal(out[j].Status.QueuedAt) {
			return out[i].Status.QueuedAt.Before(out[j].Status.QueuedAt)
		}
		return out[i].Workload.ID < out[j].Workload.ID
	})
	return out
}

// queuedCount is how many workloads are waiting, for the dashboard.
func (wr *WorkloadRegistry) queuedCount() int {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	n := 0
	for _, rec := range wr.workloads {
		if rec.Status.State == domain.WorkloadQueued {
			n++
		}
	}
	return n
}

// assignQueued moves a QUEUED workload to PENDING on target.
func (wr *WorkloadRegistry) assignQueued(id domain.WorkloadID, target domain.NodeID) (WorkloadRecord, bool) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	rec, ok := wr.workloads[id]
	if !ok || rec.Status.State != domain.WorkloadQueued {
		return WorkloadRecord{}, false
	}
	rec.Workload.Target = target
	rec.Status = domain.WorkloadStatus{ID: id, Target: target, State: domain.WorkloadPending}
	rec.cancelRequested = false
	return *rec, true
}

// markCancelRequested records an operator cancel of a PENDING workload, so
// a busy refusal that crosses it on the wire cancels rather than re-queues.
func (wr *WorkloadRegistry) markCancelRequested(id domain.WorkloadID) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if rec, ok := wr.workloads[id]; ok && rec.Status.State == domain.WorkloadPending {
		rec.cancelRequested = true
	}
}

// requeue puts a workload an agent refused for lack of slots back in the
// queue, keeping its place (QueuedAt) and backing off its next attempt.
// Only a PENDING workload (assigned, never started) can be refused, so a
// stale or forged refusal can't resurrect finished or running work. If the
// operator canceled it meanwhile, it becomes CANCELED instead.
func (wr *WorkloadRegistry) requeue(id domain.WorkloadID, now time.Time, attempts int) (WorkloadRecord, bool) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	rec, ok := wr.workloads[id]
	if !ok || rec.Status.State != domain.WorkloadPending {
		return WorkloadRecord{}, false
	}
	if rec.cancelRequested {
		rec.Status.State = domain.WorkloadCanceled
		rec.Status.Error = "canceled before it started"
		rec.Status.FinishedAt = now
		return *rec, true
	}
	queuedAt := rec.Status.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = wr.nextQueuedAtLocked(now)
	}
	if !rec.Workload.Pinned {
		rec.Workload.Target = ""
	}
	rec.Status = domain.WorkloadStatus{
		ID: id, Target: rec.Workload.Target, State: domain.WorkloadQueued,
		QueuedAt: queuedAt, NotBefore: now.Add(backoffFor(attempts, requeueBackoffBase, requeueBackoffMax)),
	}
	return *rec, true
}

// cancelQueued cancels a workload that never left the queue.
func (wr *WorkloadRegistry) cancelQueued(id domain.WorkloadID, reason string) (WorkloadRecord, bool) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	rec, ok := wr.workloads[id]
	if !ok || rec.Status.State != domain.WorkloadQueued {
		return WorkloadRecord{}, false
	}
	rec.Status.State = domain.WorkloadCanceled
	rec.Status.Error = reason
	rec.Status.FinishedAt = time.Now().UTC()
	return *rec, true
}

// kickDispatch asks the dispatcher to look at the queue now (a slot may
// have freed). Never blocks: a pending kick already covers it.
func (s *Server) kickDispatch() {
	select {
	case s.dispatchKick <- struct{}{}:
	default:
	}
}

// dispatchQueued places every queued workload that now has room, oldest
// first. Later, smaller workloads may run ahead of an older one that
// still doesn't fit anywhere — there is no head-of-line blocking.
//
// One pass holds placeMu while it decides (so no concurrent placement can
// take the same slot) and places against one usage snapshot it keeps
// current as it assigns — one registry scan per pass, not per workload
// per node, which matters once a job queues hundreds of tasks on a
// phone-hosted manager. Once an unconstrained workload of a capability
// finds no room, the pass skips the rest of that capability's
// unconstrained ones. Disk writes and sends happen after the lock is
// released, so a long pass never stalls submissions on fsyncs.
func (s *Server) dispatchQueued(ctx context.Context) {
	queued := s.Workloads.queued(time.Now())
	if len(queued) == 0 {
		return
	}
	type dispatch struct {
		conn domain.Conn
		rec  WorkloadRecord
	}
	var out []dispatch
	s.placeMu.Lock()
	usage := s.Workloads.usageByNode()
	full := map[domain.CapabilityName]bool{}
	for _, rec := range queued {
		p := placementFor(rec.Workload)
		if p.unconstrained() && full[p.capability] {
			continue
		}
		target, id, err := s.resolve(p, usage)
		if err != nil {
			if p.unconstrained() {
				full[p.capability] = true
			}
			continue // still no room (or its node is away): stays queued
		}
		assigned, ok := s.Workloads.assignQueued(rec.Workload.ID, id)
		if !ok {
			continue // canceled meanwhile
		}
		u := usage[id]
		u.add(assigned.Workload)
		usage[id] = u
		out = append(out, dispatch{target.Conn, assigned})
	}
	s.placeMu.Unlock()

	for _, d := range out {
		s.persistWorkloadRecord(d.rec)
		s.assign(ctx, d.conn, d.rec.Workload)
		log.Printf("workload.assigned: %s to %s (from the queue)", d.rec.Workload.ID, d.rec.Workload.Target)
		s.publish(domain.EventWorkloadAssigned, d.rec.Workload.Target, map[string]any{
			"workloadId": string(d.rec.Workload.ID), "command": d.rec.Workload.Command, "capability": string(d.rec.Workload.EffectiveCapability()), "fromQueue": true,
		})
	}
}

// handleBusyRefusal re-queues a workload an agent refused only because its
// slots were full, and holds new work off that node briefly (busyHoldOff)
// — otherwise every other queued workload would be sent there and refused
// in turn. The workload's own backoff (NotBefore) keeps it from spinning.
func (s *Server) handleBusyRefusal(node domain.NodeID, status domain.WorkloadStatus) {
	s.Registry.holdOffBusy(node, time.Now().Add(busyHoldOff))
	s.grants.drop(status.ID) // the next attempt gets a fresh token
	attempts := s.requeueAttempts(status.ID)
	rec, ok := s.Workloads.requeue(status.ID, time.Now().UTC(), attempts)
	if !ok {
		log.Printf("manager: ignoring a busy refusal from %s for workload %s, which is not waiting to start", node, status.ID)
		return
	}
	s.persistWorkloadRecord(rec)
	if rec.Status.State == domain.WorkloadCanceled {
		s.forgetRequeues(status.ID)
		log.Printf("workload.canceled: %s (canceled before %s had a free slot)", status.ID, node)
		s.publish(domain.EventWorkloadCanceled, node, map[string]any{"workloadId": string(status.ID)})
		return
	}
	log.Printf("workload.queued: %s (node %s had no free slot; retrying)", status.ID, node)
	s.publish(domain.EventWorkloadQueued, node, map[string]any{"workloadId": string(status.ID), "reason": "node busy"})
}

// forgetRequeues drops id's busy-refusal count once it is finished.
func (s *Server) forgetRequeues(id domain.WorkloadID) {
	s.requeueMu.Lock()
	defer s.requeueMu.Unlock()
	delete(s.requeues, id)
}

// requeueAttempts counts busy refusals per workload for its backoff.
func (s *Server) requeueAttempts(id domain.WorkloadID) int {
	s.requeueMu.Lock()
	defer s.requeueMu.Unlock()
	s.requeues[id]++
	return s.requeues[id] - 1
}
