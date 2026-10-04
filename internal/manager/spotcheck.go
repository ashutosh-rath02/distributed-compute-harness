package manager

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Spot checks (roadmap-after-9 item 20): once devices the operator
// doesn't control join, one of them could return wrong results, faulty
// or dishonest. With policy's SpotCheckPercent set, the manager re-runs a
// sample of every job's deterministic tasks (catalog Type.Deterministic)
// on a different device and compares the output hashes it computed
// itself as the files arrived — never what an agent claims. If they
// differ, a third device breaks the tie and the odd one out is marked
// suspect: a node.result-mismatch entry in the security log and a mark in
// GET /nodes and on the dashboard. Nothing acts on the mark; the operator
// decides (clear it, or revoke the device).
//
// A re-run is an ordinary workload with no job: it is never an attempt,
// so it can't change the job's state or outputs. ExcludeNodes keeps it
// off every device that already ran the task; otherwise it is placed,
// queued and held by availability like any task. The reconcile loop alone
// starts and advances checks (after advanceJobs), and each is kept as a
// small record (store bucket "spot-checks"), so the job view still says
// "matched" once the re-run workloads themselves have been forgotten.
//
// Which tasks: jobs submitted while spot checks are on (the job keeps the
// share it was submitted with, Job.SpotCheckPercent; turning them off
// stops new checks for running jobs too). Each job's deterministic tasks
// are ranked by an HMAC of job and task under a manager secret (the
// operator token) and the first SpotCheckPercent of them (at least one)
// are checked as they complete. So the sample is decided once per job,
// the same after a restart, and no device can tell whether its result
// will be checked.
//
// Deferred: re-running a task whose own device was the odd one out (its
// output may already have fed the job's reduce: the job view says so and
// the operator decides); checking render.fractal and archive.zip, which
// aren't byte-identical across devices (see catalog).

const (
	// maxSpotRuns bounds the re-runs per check: the check, a tiebreak, and
	// room for a re-run that failed or lost its device.
	maxSpotRuns = 4
	// spotStartWindow: a task can still be checked this long after its job
	// finished (the reduce, and the last task, finish the job).
	spotStartWindow = 10 * time.Minute
	// spotCheckDeadline: a check still waiting for a device after this is
	// given up.
	spotCheckDeadline = 24 * time.Hour
	// spotCheckKeep: finished checks kept, newest first.
	spotCheckKeep = 5000

	actorSpotCheck = "spot-check"
	excludedReason = "it ran the result being checked"
)

// spotCheckStore is what a store needs for spot checks to survive a
// restart (internal/store/persistent has it); without one they live in
// memory only.
type spotCheckStore interface {
	UpsertSpotCheck(c domain.SpotCheck) error
	DeleteSpotChecks(checks []domain.SpotCheck) error
	ListSpotChecks() ([]domain.SpotCheck, error)
	PutSuspect(id domain.NodeID, mark *domain.SuspectMark) error
	ListSuspects() (map[domain.NodeID]domain.SuspectMark, error)
}

type spotCheckTable struct {
	load     sync.Once
	mu       sync.Mutex
	checks   map[domain.JobID]map[string]domain.SpotCheck // by job, then task
	n        int                                          // checks held
	suspects map[domain.NodeID]domain.SuspectMark
	// samples caches each job's ranking and sampled task keys for the
	// percent they were taken at.
	samples map[domain.JobID]spotSample
	// key ranks samples when there is no operator token (tests).
	key []byte
	// suspectMu serializes a mark's read-modify-persist.
	suspectMu sync.Mutex
}

type spotSample struct {
	percent int
	ranked  []string // every deterministic task, in sampling order
	keys    map[string]bool
}

func newSpotCheckTable() *spotCheckTable {
	key := make([]byte, 32)
	rand.Read(key)
	return &spotCheckTable{checks: map[domain.JobID]map[string]domain.SpotCheck{}, suspects: map[domain.NodeID]domain.SuspectMark{}, samples: map[domain.JobID]spotSample{}, key: key}
}

// spotTable returns the table, loading it from the store the first time.
// Nil on a bare Server (unit tests).
func (s *Server) spotTable() *spotCheckTable {
	t := s.spots
	if t == nil {
		return nil
	}
	t.load.Do(func() {
		st, ok := s.store.(spotCheckStore)
		if !ok {
			return
		}
		checks, err := st.ListSpotChecks()
		if err != nil {
			log.Printf("manager: load spot checks: %v", err)
		}
		suspects, err := st.ListSuspects()
		if err != nil {
			log.Printf("manager: load suspect marks: %v", err)
		}
		for _, c := range checks {
			t.put(c)
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		for id, m := range suspects {
			t.suspects[id] = m
		}
	})
	return t
}

func (s *Server) persistSpotCheck(c domain.SpotCheck) {
	if st, ok := s.store.(spotCheckStore); ok {
		if err := st.UpsertSpotCheck(c); err != nil {
			log.Printf("manager: persist spot check %s/%s: %v", c.Job, c.Task, err)
		}
	}
}

func (t *spotCheckTable) get(job domain.JobID, task string) (domain.SpotCheck, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.checks[job][task]
	return c, ok
}

func (t *spotCheckTable) put(c domain.SpotCheck) {
	t.mu.Lock()
	defer t.mu.Unlock()
	byTask := t.checks[c.Job]
	if byTask == nil {
		byTask = map[string]domain.SpotCheck{}
		t.checks[c.Job] = byTask
	}
	if _, ok := byTask[c.Task]; !ok {
		t.n++
	}
	byTask[c.Task] = c
}

// open returns the checks still in progress, oldest first.
func (t *spotCheckTable) open() []domain.SpotCheck {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []domain.SpotCheck
	for _, byTask := range t.checks {
		for _, c := range byTask {
			if c.State == domain.SpotChecking {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (t *spotCheckTable) ofJob(job domain.JobID) []domain.SpotCheck {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]domain.SpotCheck, 0, len(t.checks[job]))
	for _, c := range t.checks[job] {
		out = append(out, c)
	}
	return out
}

// prune forgets the oldest finished checks beyond keep.
func (t *spotCheckTable) prune(keep int) []domain.SpotCheck {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n <= keep {
		return nil
	}
	var done []domain.SpotCheck
	for _, byTask := range t.checks {
		for _, c := range byTask {
			if c.State != domain.SpotChecking {
				done = append(done, c)
			}
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].CreatedAt.Before(done[j].CreatedAt) })
	n := min(t.n-keep, len(done))
	for _, c := range done[:n] {
		delete(t.checks[c.Job], c.Task)
		if len(t.checks[c.Job]) == 0 {
			delete(t.checks, c.Job)
		}
		t.n--
	}
	return done[:n]
}

// spotSampleKeys ranks job's deterministic tasks by HMAC(key, job/task)
// and returns them in that order, and the first percent of them: at
// least one (none at 0).
func spotSampleKeys(key []byte, job domain.Job, percent int) ([]string, map[string]bool) {
	var keys []string
	add := func(k string, t domain.TaskSpec) {
		if ty, ok := catalog.Lookup(t.Capability); ok && ty.Deterministic {
			keys = append(keys, k)
		}
	}
	for i, t := range job.Tasks {
		add(domain.TaskKey(i), t)
	}
	if job.Reduce != nil {
		add(domain.ReduceTask, *job.Reduce)
	}
	if percent <= 0 || len(keys) == 0 {
		return nil, nil
	}
	rank := make(map[string]string, len(keys))
	for _, k := range keys {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("harness-spot-check-v1:" + string(job.ID) + "/" + k))
		rank[k] = string(mac.Sum(nil))
	}
	sort.Slice(keys, func(i, j int) bool { return rank[keys[i]] < rank[keys[j]] })
	n := (len(keys)*min(percent, 100) + 99) / 100
	out := make(map[string]bool, n)
	for _, k := range keys[:n] {
		out[k] = true
	}
	return keys, out
}

func (s *Server) spotSample(t *spotCheckTable, job domain.Job, percent int) spotSample {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cached, ok := t.samples[job.ID]; ok && cached.percent == percent {
		return cached
	}
	key := t.key
	if s.cfg.OperatorToken != "" {
		key = []byte(s.cfg.OperatorToken)
	}
	ranked, keys := spotSampleKeys(key, job, percent)
	t.samples[job.ID] = spotSample{percent, ranked, keys}
	return t.samples[job.ID]
}

// spotCheckable: a job whose completed tasks may still get a check.
func spotCheckable(job domain.Job, now time.Time) bool {
	switch job.State {
	case domain.JobRunning:
		return true
	case domain.JobCompleted, domain.JobFailed:
		return now.Sub(job.FinishedAt) < spotStartWindow
	}
	return false
}

// advanceSpotChecks moves every open check forward, then starts checks
// for sampled tasks that have completed. Run by the reconcile loop only.
func (s *Server) advanceSpotChecks(ctx context.Context) {
	t := s.spotTable()
	if t == nil {
		return
	}
	now := time.Now().UTC()
	jobs := s.jobs.list()
	byID := make(map[domain.JobID]domain.Job, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	// Checks already started finish even if spot checks were turned off.
	for _, c := range t.open() {
		s.stepSpotCheck(ctx, t, c, byID[c.Job], now)
	}
	current := s.policy.get().SpotCheckPercent
	var attempts map[domain.JobID]map[string][]WorkloadRecord
	considered := map[domain.JobID]bool{}
	for _, job := range jobs {
		// The share the job was submitted with; turning spot checks off
		// (or lowering the share) also holds for jobs already running.
		percent := min(job.SpotCheckPercent, current)
		if percent <= 0 || !spotCheckable(job, now) {
			continue
		}
		considered[job.ID] = true
		sample := s.spotSample(t, job, percent)
		done := func(key string) *WorkloadRecord {
			if attempts == nil {
				attempts = s.Workloads.byJob()
			}
			if tp := deriveTask(attempts[job.ID][key], job.MaxAttempts); tp.state == taskDone {
				return tp.current
			}
			return nil
		}
		for _, key := range sample.ranked {
			if _, started := t.get(job.ID, key); started || !sample.keys[key] {
				continue
			}
			if attempt := done(key); attempt != nil {
				s.startSpotCheck(ctx, t, job, key, *attempt, now)
			}
		}
		// At least one per job: when it ended and none of its sampled tasks
		// completed (they failed or were canceled), check the first-ranked
		// task that did.
		if job.State != domain.JobRunning && len(t.ofJob(job.ID)) == 0 {
			for _, key := range sample.ranked {
				if attempt := done(key); attempt != nil {
					s.startSpotCheck(ctx, t, job, key, *attempt, now)
					break
				}
			}
		}
	}
	t.mu.Lock()
	for id := range t.samples {
		if !considered[id] {
			delete(t.samples, id)
		}
	}
	t.mu.Unlock()
	if dropped := t.prune(spotCheckKeep); len(dropped) > 0 {
		if st, ok := s.store.(spotCheckStore); ok {
			if err := st.DeleteSpotChecks(dropped); err != nil {
				log.Printf("manager: forget old spot checks: %v", err)
			}
		}
	}
}

// startSpotCheck records the decision to check a completed attempt
// (before acting on it, so it is made once), then starts the first re-run.
func (s *Server) startSpotCheck(ctx context.Context, t *spotCheckTable, job domain.Job, key string, attempt WorkloadRecord, now time.Time) {
	c := domain.SpotCheck{
		Job: job.ID, Task: key, Capability: attempt.Workload.EffectiveCapability(),
		Original: attempt.Workload.ID, Node: attempt.Workload.Target, Outputs: attempt.Status.Outputs,
		State: domain.SpotChecking, CreatedAt: now,
	}
	if len(c.Outputs) == 0 {
		c.State, c.Detail, c.FinishedAt = domain.SpotSkipped, "skipped: the task delivered no files to compare", now
	}
	t.put(c)
	s.persistSpotCheck(c)
	log.Printf("spotcheck.started: job %s task %s (ran on %s)", job.ID, key, c.Node)
	if c.State == domain.SpotChecking {
		s.stepSpotCheck(ctx, t, c, job, now)
	}
}

// spotVerdict is what judgeSpotCheck decides.
type spotVerdict struct {
	wait, run bool // a re-run is in flight; start another one
	state     domain.SpotCheckState
	suspects  []domain.NodeID
	detail    string
}

// judgeSpotCheck reads a check's state from its re-runs. Only COMPLETED
// re-runs count as results (their outputs as the manager received them);
// one that failed, was canceled or lost its device is no evidence either
// way, and another device is tried.
func judgeSpotCheck(c domain.SpotCheck, name func(domain.NodeID) string) spotVerdict {
	var done []domain.SpotCheckRun
	inFlight := false
	for _, r := range c.Runs {
		switch r.State {
		case domain.WorkloadCompleted:
			done = append(done, r)
		case domain.WorkloadQueued, domain.WorkloadPending, domain.WorkloadRunning:
			inFlight = true
		}
	}
	decided := func(state domain.SpotCheckState, detail string, suspects ...domain.NodeID) spotVerdict {
		return spotVerdict{state: state, detail: detail, suspects: suspects}
	}
	if len(done) == 0 {
		switch {
		case inFlight:
			return spotVerdict{wait: true}
		case len(c.Runs) >= maxSpotRuns:
			return decided(domain.SpotSkipped, fmt.Sprintf("skipped: the check didn't finish on %d other device(s)", len(c.Runs)))
		}
		return spotVerdict{run: true}
	}
	first := done[0]
	if sameOutputs(c.Outputs, first.Outputs) {
		return decided(domain.SpotMatched, "matched: "+name(first.Node)+" got the same result")
	}
	if len(done) >= 2 {
		second := done[1]
		switch {
		case sameOutputs(c.Outputs, second.Outputs):
			return decided(domain.SpotMismatch, fmt.Sprintf("mismatch: %s's result differed from %s's and %s's", name(first.Node), name(c.Node), name(second.Node)), first.Node)
		case sameOutputs(first.Outputs, second.Outputs):
			return decided(domain.SpotMismatch, fmt.Sprintf("mismatch: %s's result differed from %s's and %s's", name(c.Node), name(first.Node), name(second.Node)), c.Node)
		}
		return decided(domain.SpotUnresolved, fmt.Sprintf("mismatch, unresolved: %s, %s and %s all got different results", name(c.Node), name(first.Node), name(second.Node)))
	}
	switch {
	case inFlight:
		return spotVerdict{wait: true}
	case len(c.Runs) >= maxSpotRuns:
		return decided(domain.SpotUnresolved, fmt.Sprintf("mismatch, unresolved: %s and %s got different results and no third device finished the tiebreak", name(c.Node), name(first.Node)))
	}
	return spotVerdict{run: true}
}

// sameOutputs compares two results by file name and the hash and size the
// manager recorded on receipt.
func sameOutputs(a, b []domain.ArtifactRef) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	byName := make(map[string]domain.ArtifactRef, len(a))
	for _, o := range a {
		byName[o.Name] = o
	}
	for _, o := range b {
		if got, ok := byName[o.Name]; !ok || got.SHA256 != o.SHA256 || got.Size != o.Size {
			return false
		}
		delete(byName, o.Name) // each name once
	}
	return len(byName) == 0
}

// spotName is how a check's detail names a device: the operator's alias,
// else the agent's name (clipped; the dashboard escapes it), else its ID.
func (s *Server) spotName(id domain.NodeID) string {
	if id == "" {
		return "another device"
	}
	if name := s.nodeDisplayName(id); name != "" {
		return clip(name)
	}
	if len(id) > 12 {
		return string(id[:12])
	}
	return string(id)
}

// stepSpotCheck advances one open check: records how its re-runs went,
// then waits, starts the next re-run, or settles it.
func (s *Server) stepSpotCheck(ctx context.Context, t *spotCheckTable, c domain.SpotCheck, job domain.Job, now time.Time) {
	changed := s.refreshSpotRuns(&c)
	var v spotVerdict
	switch {
	case job.ID == "" || job.State == domain.JobCanceled:
		s.cancelSpotRuns(ctx, c)
		v = spotVerdict{state: domain.SpotSkipped, detail: "skipped: the job was canceled"}
	case now.Sub(c.CreatedAt) > spotCheckDeadline:
		s.cancelSpotRuns(ctx, c)
		v = spotVerdict{state: domain.SpotSkipped, detail: "skipped: no other device took the check within a day"}
		for _, r := range c.Runs {
			if r.State == domain.WorkloadCompleted { // differed, or it would be settled
				v = spotVerdict{state: domain.SpotUnresolved, detail: fmt.Sprintf("mismatch, unresolved: %s and %s got different results and no third device took the tiebreak within a day", s.spotName(c.Node), s.spotName(r.Node))}
				break
			}
		}
	default:
		v = judgeSpotCheck(c, s.spotName)
		if v.run {
			v = s.submitSpotRun(ctx, &c)
			changed = true
		}
	}
	if v.state != "" {
		c.State, c.Detail, c.Suspects, c.FinishedAt = v.state, v.detail, v.suspects, now
		if c.State == domain.SpotMismatch || c.State == domain.SpotUnresolved {
			s.reportSpotMismatch(t, &c)
		}
		log.Printf("spotcheck.finished: job %s task %s: %s", c.Job, c.Task, c.Detail)
		s.publish(domain.EventJobSpotCheck, "", map[string]any{"jobId": string(c.Job), "task": c.Task, "state": string(c.State)})
		changed = true
	}
	if changed {
		t.put(c)
		s.persistSpotCheck(c)
	}
}

// refreshSpotRuns copies each unfinished re-run's node and state (and, once
// it ends, its verified outputs) from its workload record.
func (s *Server) refreshSpotRuns(c *domain.SpotCheck) (changed bool) {
	for i := range c.Runs {
		r := &c.Runs[i]
		switch r.State {
		case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled, domain.WorkloadUnknown:
			continue
		}
		rec, ok := s.Workloads.Get(r.Workload)
		if !ok {
			r.State, r.Error, changed = domain.WorkloadFailed, "its workload record is gone", true
			continue
		}
		if rec.Workload.Target != r.Node || rec.Status.State != r.State {
			r.Node, r.State, changed = rec.Workload.Target, rec.Status.State, true
		}
		switch r.State {
		case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled, domain.WorkloadUnknown:
			r.Outputs, r.Error = rec.Status.Outputs, rec.Status.Error
		}
	}
	return changed
}

func (s *Server) cancelSpotRuns(ctx context.Context, c domain.SpotCheck) {
	for _, r := range c.Runs {
		switch r.State {
		case domain.WorkloadQueued, domain.WorkloadPending, domain.WorkloadRunning:
			if err := s.CancelWorkload(ctx, r.Workload); err != nil {
				log.Printf("manager: cancel spot check run %s: %v", r.Workload, err)
			}
		}
	}
}

// submitSpotRun starts the next re-run: the original attempt's own
// request (type, canonical parameters, inputs — a reduce's parts
// included), on any device that hasn't run this task yet. When no such
// device can run it the check is settled: skipped, or unresolved if two
// results already differ.
func (s *Server) submitSpotRun(ctx context.Context, c *domain.SpotCheck) spotVerdict {
	orig, ok := s.Workloads.Get(c.Original)
	if !ok {
		return spotVerdict{state: domain.SpotSkipped, detail: "skipped: the task's own record is gone"}
	}
	exclude := []domain.NodeID{c.Node}
	var differed domain.NodeID
	for _, r := range c.Runs {
		if r.Node != "" {
			exclude = append(exclude, r.Node)
		}
		if r.State == domain.WorkloadCompleted && differed == "" {
			differed = r.Node
		}
	}
	w := orig.Workload
	wl, err := s.Submit(ctx, WorkloadSpec{
		Capability: w.Capability, Params: w.Params, Requirements: w.Requirements, RestartPolicy: domain.RestartNever,
		Inputs: w.Inputs, ExcludeNodes: exclude,
	})
	if err != nil {
		noDevice := errors.Is(err, ErrNoEligibleNode) || errors.Is(err, ErrNoReadyNode)
		switch {
		case differed != "":
			return spotVerdict{state: domain.SpotUnresolved, detail: fmt.Sprintf("mismatch, unresolved: %s and %s got different results and there is no third device to tell which is right", s.spotName(c.Node), s.spotName(differed))}
		case noDevice && len(c.Runs) == 0:
			return spotVerdict{state: domain.SpotSkipped, detail: "skipped: no other device"}
		case noDevice:
			return spotVerdict{state: domain.SpotSkipped, detail: "skipped: the check didn't finish on another device and no other device can take it"}
		}
		return spotVerdict{state: domain.SpotSkipped, detail: "skipped: " + strings.TrimPrefix(err.Error(), "manager: ")}
	}
	run := domain.SpotCheckRun{Workload: wl.ID, Node: wl.Target, State: domain.WorkloadQueued}
	if rec, ok := s.Workloads.Get(wl.ID); ok {
		run.Node, run.State = rec.Workload.Target, rec.Status.State
	}
	c.Runs = append(c.Runs, run)
	log.Printf("spotcheck.run: job %s task %s re-run as %s (not on %v)", c.Job, c.Task, wl.ID, exclude)
	s.publish(domain.EventJobSpotCheck, "", map[string]any{"jobId": string(c.Job), "task": c.Task, "state": string(domain.SpotChecking)})
	return spotVerdict{}
}

// reportSpotMismatch marks the odd one out and writes the security log:
// at most one node.result-mismatch per job and device (one per job when
// nobody could be singled out), so a device that is wrong on every task
// of a large job can't flood the log the operator relies on.
func (s *Server) reportSpotMismatch(t *spotCheckTable, c *domain.SpotCheck) {
	for _, id := range c.Suspects {
		s.markSuspect(t, id, *c)
	}
	for _, other := range t.ofJob(c.Job) {
		if other.Task != c.Task && other.Audited && other.State == c.State && slices.Equal(other.Suspects, c.Suspects) {
			return
		}
	}
	nodes := []string{string(c.Node)}
	results := map[string]string{string(c.Node): outputsDigest(c.Outputs)}
	workloads := []string{string(c.Original)}
	for _, r := range c.Runs {
		if r.State == domain.WorkloadCompleted {
			nodes = append(nodes, string(r.Node))
			results[string(r.Node)] = outputsDigest(r.Outputs)
			workloads = append(workloads, string(r.Workload))
		}
	}
	var subject domain.NodeID
	if len(c.Suspects) == 1 {
		subject = c.Suspects[0]
	}
	s.audit(domain.AuditSecurity, "node.result-mismatch", subject, actorSpotCheck, map[string]any{
		"jobId": string(c.Job), "task": c.Task, "capability": string(c.Capability), "verdict": string(c.State),
		"nodes": nodes, "results": results, "workloads": workloads, "detail": c.Detail,
	})
	c.Audited = true
}

// outputsDigest names a result in the audit log: each output's name and
// the start of its hash (the files stay downloadable by hash for a while).
func outputsDigest(outs []domain.ArtifactRef) string {
	parts := make([]string, 0, len(outs))
	for _, o := range outs {
		sha := o.SHA256
		if len(sha) > 16 {
			sha = sha[:16]
		}
		parts = append(parts, o.Name+"="+sha)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// markSuspect adds a spot check's finding to a device's suspect mark.
func (s *Server) markSuspect(t *spotCheckTable, id domain.NodeID, c domain.SpotCheck) {
	if s.revocations != nil && s.revocations.has(id) {
		return // already refused for good: nothing left to warn about
	}
	t.suspectMu.Lock()
	defer t.suspectMu.Unlock()
	now := time.Now().UTC()
	t.mu.Lock()
	m, had := t.suspects[id]
	t.mu.Unlock()
	if !had {
		m.Since = now
	}
	m.Last, m.Count, m.Job, m.Task = now, m.Count+1, c.Job, c.Task
	m.Reason = fmt.Sprintf("its result for job %s task %s differed from other devices' (%s)", shortID(string(c.Job)), c.Task, c.Capability)
	if st, ok := s.store.(spotCheckStore); ok {
		if err := st.PutSuspect(id, &m); err != nil {
			log.Printf("manager: persist suspect mark for %s: %v", id, err)
		}
	}
	t.mu.Lock()
	t.suspects[id] = m
	t.mu.Unlock()
	log.Printf("node.suspect: %s (%s)", id, m.Reason)
	s.publish(domain.EventNodeUpdated, id, map[string]any{"reason": "results differed from other devices'"})
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// SuspectMark returns a node's suspect mark, if it has one.
func (s *Server) SuspectMark(id domain.NodeID) (domain.SuspectMark, bool) {
	t := s.spotTable()
	if t == nil {
		return domain.SuspectMark{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.suspects[id]
	return m, ok
}

func (s *Server) suspectView(id domain.NodeID) *domain.SuspectMark {
	if m, ok := s.SuspectMark(id); ok {
		return &m
	}
	return nil
}

// ClearSuspect removes a node's suspect mark (the operator looked into
// it); false if it had none.
func (s *Server) ClearSuspect(id domain.NodeID) (domain.SuspectMark, bool) {
	t := s.spotTable()
	if t == nil {
		return domain.SuspectMark{}, false
	}
	t.suspectMu.Lock()
	defer t.suspectMu.Unlock()
	t.mu.Lock()
	m, ok := t.suspects[id]
	t.mu.Unlock()
	if !ok {
		return m, false
	}
	if st, ok := s.store.(spotCheckStore); ok {
		if err := st.PutSuspect(id, nil); err != nil {
			log.Printf("manager: clear suspect mark for %s: %v", id, err)
		}
	}
	t.mu.Lock()
	delete(t.suspects, id)
	t.mu.Unlock()
	return m, true
}

// apiClearSuspect is DELETE /nodes/{id}/suspect.
func (s *Server) apiClearSuspect(w http.ResponseWriter, r *http.Request) {
	id := domain.NodeID(r.PathValue("id"))
	m, ok := s.ClearSuspect(id)
	if !ok {
		http.Error(w, "this device has no suspect mark", http.StatusNotFound)
		return
	}
	s.audit(domain.AuditSecurity, "node.suspect-cleared", id, actorFrom(r.Context()), map[string]any{"count": m.Count, "jobId": string(m.Job), "task": m.Task})
	s.publish(domain.EventNodeUpdated, id, map[string]any{"reason": "suspect mark cleared"})
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

// spotCheckLabel is a task's spot check as the job view shows it ("" when
// it isn't checked).
func (s *Server) spotCheckLabel(job domain.JobID, task string) string {
	t := s.spotTable()
	if t == nil {
		return ""
	}
	c, ok := t.get(job, task)
	if !ok {
		return ""
	}
	if c.State != domain.SpotChecking {
		return c.Detail
	}
	for _, r := range c.Runs {
		if r.State == domain.WorkloadCompleted {
			return "checking: the results differed; a third device is breaking the tie"
		}
	}
	return "checking"
}

// spotCheckCounts summarizes a job's spot checks (nil when it has none).
type spotCheckCounts struct {
	Checking   int `json:"checking,omitempty"`
	Matched    int `json:"matched,omitempty"`
	Mismatch   int `json:"mismatch,omitempty"`
	Unresolved int `json:"unresolved,omitempty"`
	Skipped    int `json:"skipped,omitempty"`
}

func (s *Server) spotCheckCounts(job domain.JobID) *spotCheckCounts {
	t := s.spotTable()
	if t == nil {
		return nil
	}
	checks := t.ofJob(job)
	if len(checks) == 0 {
		return nil
	}
	var n spotCheckCounts
	for _, c := range checks {
		switch c.State {
		case domain.SpotChecking:
			n.Checking++
		case domain.SpotMatched:
			n.Matched++
		case domain.SpotMismatch:
			n.Mismatch++
		case domain.SpotUnresolved:
			n.Unresolved++
		case domain.SpotSkipped:
			n.Skipped++
		}
	}
	return &n
}
