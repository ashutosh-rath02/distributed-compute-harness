package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Batch jobs (roadmap item 6): one request fanned out as many tasks
// spread over the fleet, each retried on failure, optionally fanned back
// in by a reduce task that receives every task's outputs.
//
// Every attempt is an ordinary workload (queueing, slots, files, transfer
// tokens all apply unchanged) carrying Job/Task/Attempt. The job record
// holds only the request and the final outcome; which attempt a task is
// on, how many attempts it has used, where they failed, and its outputs
// are all derived from the attempts' own workload records each pass — so
// a crash between "submitted an attempt" and "updated the job" can't lose
// or duplicate a retry, and a 1000-task job isn't rewritten on every
// task transition.
//
// One goroutine (the reconcile loop) advances jobs and is the only one
// that submits attempts.

// maxNodeLostRetries: attempts lost to the node going away (or to a
// manager restart) don't count against MaxAttempts — up to this many, so
// a task that reliably takes its device down can't retry forever.
const maxNodeLostRetries = 5

// maxAttemptsPerPass bounds how many attempts one pass submits, so a
// 1000-task job doesn't hold the reconcile goroutine (and its fsyncs) for
// the whole fan-out at once; the pass kicks the next one to continue.
const maxAttemptsPerPass = 100

type jobTable struct {
	mu   sync.Mutex
	jobs map[domain.JobID]*domain.Job
	// sweep: canceled jobs that may still have an attempt in flight
	// (one submitted concurrently with the cancel); the next pass cancels
	// it.
	sweep map[domain.JobID]bool
	// waiting explains, per job and task key, why a task has no attempt
	// yet (the last submit error). Display only.
	waiting map[domain.JobID]map[string]string
}

func newJobTable() *jobTable {
	return &jobTable{jobs: map[domain.JobID]*domain.Job{}, sweep: map[domain.JobID]bool{}, waiting: map[domain.JobID]map[string]string{}}
}

func (t *jobTable) put(j domain.Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := j
	t.jobs[j.ID] = &c
}

func (t *jobTable) get(id domain.JobID) (domain.Job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		return domain.Job{}, false
	}
	return *j, true
}

func (t *jobTable) list() []domain.Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]domain.Job, 0, len(t.jobs))
	for _, j := range t.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })
	return out
}

// finish moves a RUNNING job to a final state; false if it already was.
func (t *jobTable) finish(id domain.JobID, state domain.JobState, reason string) (domain.Job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok || j.State != domain.JobRunning {
		return domain.Job{}, false
	}
	j.State, j.Error, j.FinishedAt = state, reason, time.Now().UTC()
	delete(t.waiting, id)
	if state == domain.JobCanceled {
		t.sweep[id] = true
	}
	return *j, true
}

func (t *jobTable) running(id domain.JobID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	return ok && j.State == domain.JobRunning
}

func (t *jobTable) setWaiting(id domain.JobID, key, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waiting[id] == nil {
		t.waiting[id] = map[string]string{}
	}
	if reason == "" {
		delete(t.waiting[id], key)
	} else {
		t.waiting[id][key] = reason
	}
}

func (t *jobTable) waitingReason(id domain.JobID, key string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waiting[id][key]
}

// byJob groups every job attempt by job and task key, oldest attempt
// first — one registry scan for a whole pass.
func (wr *WorkloadRegistry) byJob() map[domain.JobID]map[string][]WorkloadRecord {
	wr.mu.RLock()
	out := map[domain.JobID]map[string][]WorkloadRecord{}
	for _, rec := range wr.workloads {
		if rec.Workload.Job == "" {
			continue
		}
		tasks := out[rec.Workload.Job]
		if tasks == nil {
			tasks = map[string][]WorkloadRecord{}
			out[rec.Workload.Job] = tasks
		}
		tasks[rec.Workload.Task] = append(tasks[rec.Workload.Task], *rec)
	}
	wr.mu.RUnlock()
	for _, tasks := range out {
		for _, attempts := range tasks {
			sort.Slice(attempts, func(i, k int) bool { return attempts[i].Workload.Attempt < attempts[k].Workload.Attempt })
		}
	}
	return out
}

// Task progress states, derived from a task's attempts.
const (
	taskWaiting  = "WAITING" // no attempt yet, or due another one
	taskActive   = "ACTIVE"  // an attempt is queued, assigned, or running
	taskDone     = "COMPLETED"
	taskFailed   = "FAILED" // out of attempts
	taskCanceled = "CANCELED"
)

type taskProgress struct {
	state    string
	attempts int // attempts made
	counted  int // attempts that count against MaxAttempts
	avoid    []domain.NodeID
	current  *WorkloadRecord
	outputs  []domain.ArtifactRef
	err      string
}

// deriveTask reads a task's state from its attempts (oldest first).
// MaxAttempts counts every run, the first included. A FAILED attempt
// counts and its node is avoided next time; one lost to the node going
// away or to a manager restart (NodeLost, UNKNOWN) does not — up to
// maxNodeLostRetries of them.
func deriveTask(attempts []WorkloadRecord, maxAttempts int) taskProgress {
	tp := taskProgress{attempts: len(attempts)}
	lost := 0
	for i := range attempts {
		a := &attempts[i]
		switch {
		case a.Status.State == domain.WorkloadUnknown, a.Status.State == domain.WorkloadFailed && a.Status.NodeLost:
			lost++
		case a.Status.State == domain.WorkloadFailed:
			tp.counted++
			if !slices.Contains(tp.avoid, a.Workload.Target) {
				tp.avoid = append(tp.avoid, a.Workload.Target)
			}
		}
	}
	if lost > maxNodeLostRetries {
		tp.counted += lost - maxNodeLostRetries
	}
	if len(attempts) == 0 {
		tp.state = taskWaiting
		return tp
	}
	cur := &attempts[len(attempts)-1]
	tp.current = cur
	switch cur.Status.State {
	case domain.WorkloadQueued, domain.WorkloadPending, domain.WorkloadRunning:
		tp.state = taskActive
	case domain.WorkloadCompleted:
		tp.state, tp.outputs = taskDone, cur.Status.Outputs
	case domain.WorkloadCanceled:
		tp.state, tp.err = taskCanceled, cur.Status.Error
	default: // FAILED or UNKNOWN
		tp.err = cur.Status.Error
		if tp.counted >= maxAttempts {
			tp.state = taskFailed
		} else {
			tp.state = taskWaiting
		}
	}
	return tp
}

// JobSpec is one job submission.
type JobSpec struct {
	Name        string
	Tasks       []domain.TaskSpec
	Reduce      *domain.TaskSpec
	MaxAttempts int
	// Priority is every attempt's (empty = normal).
	Priority domain.Priority
	// Workflow and Stage link a workflow stage's job to its workflow
	// (workflows.go).
	Workflow domain.WorkflowID
	Stage    int
	// ahead: the tasks' inputs don't exist yet (a later workflow stage,
	// checked before the stages before it have run). checkJob compiles
	// and places them as placeholders, like a reduce's parts, and never
	// looks them up in the store.
	ahead bool
}

// SubmitJob validates and records a job; the reconcile loop submits its
// attempts (kicked right away). Everything that would make the job
// impossible — bad files, a task or the reduce no ready node could ever
// run, a reduce whose inputs couldn't all fit — is refused here, before
// any work starts.
func (s *Server) SubmitJob(ctx context.Context, spec JobSpec) (domain.Job, error) {
	if err := s.checkJob(&spec); err != nil {
		return domain.Job{}, err
	}
	id, err := newRandomID()
	if err != nil {
		return domain.Job{}, err
	}
	job := domain.Job{
		ID: domain.JobID(id), Name: spec.Name, Tasks: spec.Tasks, Reduce: spec.Reduce,
		MaxAttempts: spec.MaxAttempts, State: domain.JobRunning, CreatedAt: time.Now().UTC(), Priority: spec.Priority,
		Workflow: spec.Workflow, Stage: spec.Stage,
	}
	if s.store != nil {
		if err := s.store.UpsertJob(job); err != nil {
			return domain.Job{}, fmt.Errorf("manager: persist job: %w", err)
		}
	}
	s.jobs.put(job)
	log.Printf("job.submitted: %s (%d tasks%s)", job.ID, len(job.Tasks), map[bool]string{true: " + reduce"}[job.Reduce != nil])
	s.publish(domain.EventJobSubmitted, "", map[string]any{"jobId": string(job.ID), "tasks": len(job.Tasks)})
	s.kickDispatch()
	return job, nil
}

// checkJob validates spec as SubmitJob does, completing it in place
// (capabilities, compiled parameters, input sizes, attempts) — the AI
// planner runs it on a proposal before anyone approves it.
func (s *Server) checkJob(spec *JobSpec) error {
	if len(spec.Tasks) == 0 || len(spec.Tasks) > domain.MaxJobTasks {
		return fmt.Errorf("%w: a job needs 1-%d tasks", ErrInvalidWorkload, domain.MaxJobTasks)
	}
	if spec.MaxAttempts == 0 {
		spec.MaxAttempts = domain.DefaultJobRetries
	}
	if spec.MaxAttempts < 1 || spec.MaxAttempts > domain.MaxJobAttempts {
		return fmt.Errorf("%w: maxAttempts must be 1-%d", ErrInvalidWorkload, domain.MaxJobAttempts)
	}
	priority, err := domain.ParsePriority(string(spec.Priority))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidWorkload, err)
	}
	spec.Priority = priority
	usage := s.Workloads.usageByNode()
	// check validates one task as Submit will (policy, typed compile,
	// files) and that some node could ever run it. parts stands in for a
	// reduce's future inputs.
	check := func(label string, t *domain.TaskSpec, parts []domain.ArtifactRef) error {
		if t.Capability == "" {
			t.Capability = domain.CapabilitySystemExecute
		}
		if err := s.checkPolicy(t.Capability); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if t.Capability == domain.CapabilitySystemExecute && t.Command == "" {
			return fmt.Errorf("%w: %s: command is required", ErrInvalidWorkload, label)
		}
		if err := compileTyped(&t.Capability, &t.Command, &t.Args, &t.Params, append(append([]domain.ArtifactRef{}, t.Inputs...), parts...), &t.Outputs, &t.Requirements); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if err := s.checkContainer(t.Capability, t.Params, &t.Requirements); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		ws := WorkloadSpec{Target: t.Target, Capability: t.Capability, Inputs: t.Inputs, Outputs: t.Outputs}
		if err := s.prepareFiles(&ws); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		t.Inputs = ws.Inputs
		// Params too: placement matches a model parameter against the
		// devices' models (without them every model task read as model "").
		w := domain.Workload{Target: t.Target, Pinned: t.Target != "", Capability: t.Capability, Params: t.Params, Requirements: t.Requirements, Inputs: append(t.Inputs, parts...), Outputs: t.Outputs, TimeoutSeconds: s.policyFor(t.Capability).MaxRuntimeSeconds}
		if _, _, err := s.resolve(placementFor(w), usage); err != nil && !errors.Is(err, errNoRoom) {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}
	for i := range spec.Tasks {
		t, label := &spec.Tasks[i], "task "+domain.TaskKey(i)
		if !spec.ahead {
			if err := check(label, t, nil); err != nil {
				return err
			}
			continue
		}
		ahead := t.Inputs
		t.Inputs = nil
		err := check(label, t, ahead)
		t.Inputs = ahead
		if err != nil {
			return err
		}
		if err := domain.ValidateWorkloadFiles(ahead, t.Outputs); err != nil {
			return fmt.Errorf("%s: %w: %v", label, ErrInvalidWorkload, err)
		}
	}
	if spec.Reduce != nil {
		// The reduce's inputs are its own plus every task's outputs at
		// parts/<task>/<name>: check now that they all fit, not after the
		// whole job has run.
		r := *spec.Reduce
		parts := append([]domain.ArtifactRef{}, r.Inputs...)
		placeholder := strings.Repeat("0", 64)
		for i, t := range spec.Tasks {
			for _, out := range t.Outputs {
				parts = append(parts, domain.ArtifactRef{Name: domain.ReducePartName(domain.TaskKey(i), out), SHA256: placeholder})
			}
		}
		if namesParts(r.Capability) { // taskindex.go
			parts = append(parts, domain.ArtifactRef{Name: catalog.TaskIndexName, SHA256: placeholder})
		}
		if err := check("reduce", &r, parts[len(r.Inputs):]); err != nil {
			return err
		}
		if err := domain.ValidateWorkloadFiles(parts, r.Outputs); err != nil {
			return fmt.Errorf("%w: reduce: %v", ErrInvalidWorkload, err)
		}
		spec.Reduce = &r
	}
	return nil
}

// CancelJob stops a job: it is marked CANCELED first (so nothing is
// retried), then every in-flight attempt is canceled.
func (s *Server) CancelJob(ctx context.Context, id domain.JobID) error {
	if _, ok := s.jobs.get(id); !ok {
		return ErrUnknownJob
	}
	job, ok := s.jobs.finish(id, domain.JobCanceled, "canceled by the operator")
	if !ok {
		return nil // already finished
	}
	s.persistJob(job)
	s.publish(domain.EventJobFinished, "", map[string]any{"jobId": string(id), "state": string(job.State)})
	s.cancelJobAttempts(ctx, id, s.Workloads.byJob()[id])
	return nil
}

// ErrUnknownJob is a job ID the manager doesn't know.
var ErrUnknownJob = errors.New("manager: unknown job")

func (s *Server) cancelJobAttempts(ctx context.Context, id domain.JobID, byTask map[string][]WorkloadRecord) (inFlight int) {
	for _, attempts := range byTask {
		for _, a := range attempts {
			switch a.Status.State {
			case domain.WorkloadQueued, domain.WorkloadPending, domain.WorkloadRunning:
				inFlight++
				if err := s.CancelWorkload(ctx, a.Workload.ID); err != nil {
					log.Printf("manager: cancel job %s attempt %s: %v", id, a.Workload.ID, err)
				}
			}
		}
	}
	return inFlight
}

func (s *Server) persistJob(job domain.Job) {
	if s.store == nil {
		return
	}
	if err := s.store.UpsertJob(job); err != nil {
		log.Printf("manager: persist job %s: %v", job.ID, err)
	}
}

// advanceJobs moves every running job forward one step: submits first
// attempts and retries, the reduce once every task is done, and records
// the outcome once nothing is left in flight.
func (s *Server) advanceJobs(ctx context.Context) {
	jobs := s.jobs.list()
	if len(jobs) == 0 {
		return
	}
	attempts := s.Workloads.byJob()
	budget := maxAttemptsPerPass
	for _, job := range jobs {
		switch job.State {
		case domain.JobRunning:
			s.advanceJob(ctx, job, attempts[job.ID], &budget)
		case domain.JobCanceled:
			s.jobs.mu.Lock()
			pending := s.jobs.sweep[job.ID]
			s.jobs.mu.Unlock()
			if pending && s.cancelJobAttempts(ctx, job.ID, attempts[job.ID]) == 0 {
				s.jobs.mu.Lock()
				delete(s.jobs.sweep, job.ID)
				s.jobs.mu.Unlock()
			}
		}
	}
	if budget <= 0 {
		s.kickDispatch() // more to submit: continue right after this pass
	}
}

func (s *Server) advanceJob(ctx context.Context, job domain.Job, byTask map[string][]WorkloadRecord, budget *int) {
	active, failed, canceled := 0, 0, 0
	var outputs [][]domain.ArtifactRef
	for i := range job.Tasks {
		key := domain.TaskKey(i)
		tp := deriveTask(byTask[key], job.MaxAttempts)
		switch tp.state {
		case taskWaiting:
			if *budget > 0 {
				*budget--
				s.submitAttempt(ctx, job, key, job.Tasks[i], nil, tp)
			}
			active++
		case taskActive:
			active++
		case taskFailed:
			failed++
		case taskCanceled:
			canceled++
		case taskDone:
			outputs = append(outputs, tp.outputs)
		}
	}
	if active > 0 {
		return
	}
	if failed > 0 || canceled > 0 {
		s.finishJob(job, domain.JobFailed, fmt.Sprintf("%d task(s) failed, %d canceled, %d completed", failed, canceled, len(outputs)))
		return
	}
	if job.Reduce == nil {
		s.finishJob(job, domain.JobCompleted, "")
		return
	}
	rp := deriveTask(byTask[domain.ReduceTask], job.MaxAttempts)
	switch rp.state {
	case taskWaiting:
		parts := append([]domain.ArtifactRef{}, job.Reduce.Inputs...)
		for i, outs := range outputs {
			for _, o := range outs {
				parts = append(parts, domain.ArtifactRef{Name: domain.ReducePartName(domain.TaskKey(i), o.Name), SHA256: o.SHA256, Size: o.Size})
			}
		}
		if *budget > 0 {
			if namesParts(job.Reduce.Capability) { // taskindex.go
				index, err := s.storeTaskIndex(job)
				if err != nil {
					s.jobs.setWaiting(job.ID, domain.ReduceTask, err.Error())
					return
				}
				parts = append(parts, index)
			}
			*budget--
			s.submitAttempt(ctx, job, domain.ReduceTask, *job.Reduce, parts, rp)
		}
	case taskDone:
		s.finishJob(job, domain.JobCompleted, "")
	case taskFailed, taskCanceled:
		s.finishJob(job, domain.JobFailed, "reduce: "+rp.err)
	}
}

// submitAttempt starts a task's next attempt. inputs overrides the
// spec's (the reduce's parts). A submit that can't be placed at all right
// now (no ready node) is retried next pass.
func (s *Server) submitAttempt(ctx context.Context, job domain.Job, key string, t domain.TaskSpec, inputs []domain.ArtifactRef, tp taskProgress) {
	if !s.jobs.running(job.ID) {
		return // canceled meanwhile
	}
	if inputs == nil {
		inputs = t.Inputs
	}
	spec := WorkloadSpec{
		Target: t.Target, Command: t.Command, Args: t.Args, Capability: t.Capability, Params: t.Params,
		Requirements: t.Requirements, RestartPolicy: domain.RestartNever, Inputs: inputs, Outputs: t.Outputs,
		Job: job.ID, Task: key, Attempt: tp.attempts + 1, AvoidNodes: tp.avoid, Priority: job.Priority,
	}
	if _, typed := catalog.Lookup(t.Capability); typed {
		spec.Outputs = nil // the type names them again for these inputs
	}
	_, err := s.Submit(ctx, spec)
	if errors.Is(err, ErrInvalidWorkload) || errors.Is(err, ErrPolicy) {
		// Won't get better by waiting (policy changed, or the reduce's
		// real inputs don't fit): stop the job and say why.
		s.finishJob(job, domain.JobFailed, "task "+key+": "+err.Error())
		return
	}
	if err != nil {
		s.jobs.setWaiting(job.ID, key, err.Error())
		return
	}
	s.jobs.setWaiting(job.ID, key, "")
}

func (s *Server) finishJob(job domain.Job, state domain.JobState, reason string) {
	done, ok := s.jobs.finish(job.ID, state, reason)
	if !ok {
		return
	}
	s.persistJob(done)
	log.Printf("job.finished: %s %s %s", done.ID, done.State, reason)
	s.publish(domain.EventJobFinished, "", map[string]any{"jobId": string(done.ID), "state": string(done.State)})
}

// jobLiveArtifacts adds what running jobs still need to live: every
// task's and the reduce's inputs (a retry fetches them again), and the
// outputs of finished tasks (the reduce's future inputs).
func (s *Server) jobLiveArtifacts(live map[string]bool) {
	var running []domain.Job
	for _, j := range s.jobs.list() {
		if j.State == domain.JobRunning {
			running = append(running, j)
		}
	}
	if len(running) == 0 {
		return
	}
	attempts := s.Workloads.byJob()
	for _, j := range running {
		specs := append([]domain.TaskSpec{}, j.Tasks...)
		if j.Reduce != nil {
			specs = append(specs, *j.Reduce)
		}
		for _, t := range specs {
			for _, in := range t.Inputs {
				live[in.SHA256] = true
			}
		}
		for _, as := range attempts[j.ID] {
			for _, a := range as {
				for _, o := range a.Status.Outputs {
					live[o.SHA256] = true
				}
			}
		}
	}
}
