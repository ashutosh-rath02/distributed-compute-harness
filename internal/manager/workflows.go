package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Multi-step workflows (roadmap item 15): an ordered list of stages, each
// an ordinary job whose tasks work on the results of the stage before
// it — resize photos, zip them, checksum the zip; render a picture in
// strips, stack them, resize the result.
//
// Like a job and its attempts, a workflow record holds only the request
// and the final outcome. Each stage's job carries the workflow's ID and
// its stage number (Job.Workflow, Job.Stage), and where a workflow stands
// is derived from those jobs each pass, so a crash between "submitted a
// stage's job" and anything after it can't lose or repeat a stage. One
// goroutine (the reconcile loop) advances workflows and is the only one
// that submits their jobs.
//
// The whole workflow is checked before anything runs: every stage is
// built as the job it will be, with placeholders where the results of the
// stages before it will be, and checked as SubmitJob checks a job (types
// and parameters, file names and counts, policy, a device able to run
// it). Stages are catalog task types only: their results' names are known
// up front, and raw commands get no new way in.

const placeholderSHA = "0000000000000000000000000000000000000000000000000000000000000000"

type workflowTable struct {
	mu        sync.Mutex
	workflows map[domain.WorkflowID]*domain.Workflow
	// waiting explains why the next stage's job isn't submitted yet (the
	// last submit error). Display only.
	waiting map[domain.WorkflowID]string
}

func newWorkflowTable() *workflowTable {
	return &workflowTable{workflows: map[domain.WorkflowID]*domain.Workflow{}, waiting: map[domain.WorkflowID]string{}}
}

func (t *workflowTable) put(wf domain.Workflow) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := wf
	t.workflows[wf.ID] = &c
}

func (t *workflowTable) get(id domain.WorkflowID) (domain.Workflow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	wf, ok := t.workflows[id]
	if !ok {
		return domain.Workflow{}, false
	}
	return *wf, true
}

// list returns every workflow, oldest first.
func (t *workflowTable) list() []domain.Workflow {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]domain.Workflow, 0, len(t.workflows))
	for _, wf := range t.workflows {
		out = append(out, *wf)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })
	return out
}

// finish moves a RUNNING workflow to a final state; false if it already was.
func (t *workflowTable) finish(id domain.WorkflowID, state domain.JobState, reason string) (domain.Workflow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	wf, ok := t.workflows[id]
	if !ok || wf.State != domain.JobRunning {
		return domain.Workflow{}, false
	}
	wf.State, wf.Error, wf.FinishedAt = state, reason, time.Now().UTC()
	delete(t.waiting, id)
	return *wf, true
}

func (t *workflowTable) running(id domain.WorkflowID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wf, ok := t.workflows[id]
	return ok && wf.State == domain.JobRunning
}

func (t *workflowTable) setWaiting(id domain.WorkflowID, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if reason == "" {
		delete(t.waiting, id)
	} else {
		t.waiting[id] = reason
	}
}

func (t *workflowTable) waitingReason(id domain.WorkflowID) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waiting[id]
}

// WorkflowSpec is one workflow submission.
type WorkflowSpec struct {
	Name string
	// Inputs are the first stage's files (stored artifacts).
	Inputs      []domain.ArtifactRef
	Stages      []domain.WorkflowStage
	MaxAttempts int
}

// ErrUnknownWorkflow is a workflow ID the manager doesn't know.
var ErrUnknownWorkflow = errors.New("manager: unknown workflow")

// SubmitWorkflow checks a whole workflow and records it; the reconcile
// loop submits its first stage's job (kicked right away).
func (s *Server) SubmitWorkflow(ctx context.Context, spec WorkflowSpec) (domain.Workflow, error) {
	if _, err := s.checkWorkflow(&spec); err != nil {
		return domain.Workflow{}, err
	}
	id, err := newRandomID()
	if err != nil {
		return domain.Workflow{}, err
	}
	wf := domain.Workflow{
		ID: domain.WorkflowID(id), Name: spec.Name, Inputs: spec.Inputs, Stages: spec.Stages,
		MaxAttempts: spec.MaxAttempts, State: domain.JobRunning, CreatedAt: time.Now().UTC(),
	}
	if wf.Name == "" {
		wf.Name = workflowTitle(wf.Stages)
	}
	if s.store != nil {
		if err := s.store.UpsertWorkflow(wf); err != nil {
			return domain.Workflow{}, fmt.Errorf("manager: persist workflow: %w", err)
		}
	}
	s.workflows.put(wf)
	log.Printf("workflow.submitted: %s (%d steps, %d files)", wf.ID, len(wf.Stages), len(wf.Inputs))
	s.publish(domain.EventWorkflowSubmitted, "", map[string]any{"workflowId": string(wf.ID), "stages": len(wf.Stages)})
	s.kickDispatch()
	return wf, nil
}

// stepTitle is a step's catalog title (its type name if unknown).
func stepTitle(step domain.WorkflowStep) string {
	if t, ok := catalog.Lookup(step.Type); ok {
		return t.Title
	}
	return string(step.Type)
}

// stageLabel names stage i (from 0) in messages: "step 2 (Zip files together)".
func stageLabel(i int, st domain.WorkflowStage) string {
	return fmt.Sprintf("step %d (%s)", i+1, stepTitle(st.WorkflowStep))
}

// workflowTitle is a name for a workflow given none: its steps in order.
func workflowTitle(stages []domain.WorkflowStage) string {
	var b strings.Builder
	for i, st := range stages {
		title := stepTitle(st.WorkflowStep)
		if i > 0 {
			b.WriteString(", then ")
			title = strings.ToLower(title[:1]) + title[1:]
		}
		b.WriteString(title)
	}
	return b.String()
}

// checkStep checks that step is a catalog task type a workflow may run:
// typed (so its results' names are known), placed by the manager (not
// tied to one named device) and not part of something the manager runs.
func checkStep(step domain.WorkflowStep) (catalog.Type, error) {
	t, ok := catalog.Lookup(step.Type)
	switch {
	case step.Type == "":
		return t, fmt.Errorf("%w: say which task type it runs", ErrInvalidWorkload)
	case !ok:
		return t, fmt.Errorf("%w: %q isn't a built-in task type (a workflow's steps are task types from the catalog: harnessctl tasks)", ErrInvalidWorkload, step.Type)
	case t.Internal || t.TargetRequired:
		return t, fmt.Errorf("%w: %s works on one named device, so it can't be a workflow step", ErrInvalidWorkload, t.Name)
	}
	return t, nil
}

// checkStage checks one stage's template (mode, parts, combine),
// normalizing an empty mode to once.
func checkStage(st *domain.WorkflowStage) error {
	t, err := checkStep(st.WorkflowStep)
	if err != nil {
		return err
	}
	if st.Mode == "" {
		st.Mode = domain.StageOnce
	}
	switch st.Mode {
	case domain.StageOnce, domain.StagePerFile:
		if st.Parts != 0 {
			return fmt.Errorf("%w: parts is only for mode %q", ErrInvalidWorkload, domain.StageParts)
		}
	case domain.StageParts:
		if !t.Parts {
			return fmt.Errorf("%w: %s can't be split into parts", ErrInvalidWorkload, t.Name)
		}
		if st.Parts < 1 || st.Parts > domain.MaxJobTasks {
			return fmt.Errorf("%w: parts must be 1-%d, not %d", ErrInvalidWorkload, domain.MaxJobTasks, st.Parts)
		}
		if _, set := st.Params["part"]; set {
			return fmt.Errorf("%w: the step's parts sets part and parts; leave them out of params", ErrInvalidWorkload)
		}
		if _, set := st.Params["parts"]; set {
			return fmt.Errorf("%w: the step's parts sets part and parts; leave them out of params", ErrInvalidWorkload)
		}
	default:
		return fmt.Errorf("%w: unknown mode %q (once, perFile or parts)", ErrInvalidWorkload, st.Mode)
	}
	if st.Combine != nil {
		ct, err := checkStep(*st.Combine)
		if err != nil {
			return fmt.Errorf("combine: %w", err)
		}
		if ct.Inputs.Max == 0 {
			return fmt.Errorf("%w: combine: %s takes no files, so it can't combine results", ErrInvalidWorkload, ct.Name)
		}
	}
	return nil
}

// checkWorkflow validates spec as SubmitWorkflow does, normalizing it in
// place, and returns each stage's checked job (later stages with
// placeholder files). Stage i+1 is built over the names stage i will hand
// on, so a stage that can't take what the one before makes is refused
// now, not after the work before it ran. What only the real results can
// tell (an archive's size against the file limit) is checked when the
// stage starts, as for a job's reduce.
func (s *Server) checkWorkflow(spec *WorkflowSpec) ([]JobSpec, error) {
	if n := len(spec.Stages); n == 0 || n > domain.MaxWorkflowStages {
		return nil, fmt.Errorf("%w: a workflow needs 1-%d steps", ErrInvalidWorkload, domain.MaxWorkflowStages)
	}
	if len(spec.Inputs) > domain.MaxJobTasks {
		return nil, fmt.Errorf("%w: at most %d files", ErrInvalidWorkload, domain.MaxJobTasks)
	}
	if spec.MaxAttempts == 0 {
		spec.MaxAttempts = domain.DefaultJobRetries
	}
	if spec.MaxAttempts < 1 || spec.MaxAttempts > domain.MaxJobAttempts {
		return nil, fmt.Errorf("%w: maxAttempts must be 1-%d", ErrInvalidWorkload, domain.MaxJobAttempts)
	}
	draft := domain.Workflow{Stages: spec.Stages, MaxAttempts: spec.MaxAttempts}
	files := spec.Inputs
	checked := make([]JobSpec, 0, len(spec.Stages))
	for i := range spec.Stages {
		label := stageLabel(i, spec.Stages[i])
		if err := checkStage(&spec.Stages[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		st := spec.Stages[i]
		switch {
		case i > 0 && len(files) == 0:
			return nil, fmt.Errorf("%s: %w: nothing to work on: step %d makes no files", label, ErrInvalidWorkload, i)
		case st.Mode == domain.StagePerFile && len(files) == 0:
			return nil, fmt.Errorf("%s: %w: it runs once per file, but the workflow has no files", label, ErrInvalidWorkload)
		}
		job := stageJob(draft, i, files)
		job.ahead = i > 0
		if err := s.checkJob(&job); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		checked = append(checked, job)
		// What this stage will hand on, by name.
		var reduce []domain.ArtifactRef
		if job.Reduce != nil {
			reduce = placeholders(job.Reduce.Outputs)
		}
		tasks := make([][]domain.ArtifactRef, len(job.Tasks))
		for k, t := range job.Tasks {
			tasks[k] = placeholders(t.Outputs)
		}
		files = handOn(job.Reduce != nil, reduce, tasks)
	}
	return checked, nil
}

func placeholders(names []string) []domain.ArtifactRef {
	out := make([]domain.ArtifactRef, len(names))
	for i, n := range names {
		out[i] = domain.ArtifactRef{Name: n, SHA256: placeholderSHA}
	}
	return out
}

// handOn names a finished stage's results for the stage after it (and,
// for the last stage, as the workflow's results): a combine step's
// outputs as they are, a single task's as they are, several tasks' at
// parts/<task>/<name> — the layout a job's combine step gets. So two
// tasks' results never collide (every file.hash run writes hashes.txt),
// and strips keep their order: image.stack joins in name order, and
// fractal-10.png sorts before fractal-2.png while task keys are
// zero-padded.
func handOn(hasReduce bool, reduce []domain.ArtifactRef, tasks [][]domain.ArtifactRef) []domain.ArtifactRef {
	if hasReduce {
		return append([]domain.ArtifactRef(nil), reduce...)
	}
	if len(tasks) == 1 {
		return append([]domain.ArtifactRef(nil), tasks[0]...)
	}
	var out []domain.ArtifactRef
	for i, outs := range tasks {
		for _, o := range outs {
			out = append(out, domain.ArtifactRef{Name: domain.ReducePartName(domain.TaskKey(i), o.Name), SHA256: o.SHA256, Size: o.Size})
		}
	}
	return out
}

// stageResults is what a COMPLETED stage job hands on, read from its
// attempts.
func stageResults(job domain.Job, byTask map[string][]WorkloadRecord) []domain.ArtifactRef {
	var reduce []domain.ArtifactRef
	if job.Reduce != nil {
		reduce = deriveTask(byTask[domain.ReduceTask], job.MaxAttempts).outputs
	}
	tasks := make([][]domain.ArtifactRef, len(job.Tasks))
	for i := range job.Tasks {
		tasks[i] = deriveTask(byTask[domain.TaskKey(i)], job.MaxAttempts).outputs
	}
	return handOn(job.Reduce != nil, reduce, tasks)
}

// stageJob builds stage i's job over files, fresh from the stage's
// template: one task per file, one with them all, or one per part, then
// the stage's combine step, if any.
func stageJob(wf domain.Workflow, i int, files []domain.ArtifactRef) JobSpec {
	st := wf.Stages[i]
	title := stepTitle(st.WorkflowStep)
	name := title
	if st.Name != "" {
		name = st.Name
	}
	spec := JobSpec{
		Name:        fmt.Sprintf("%s (workflow step %d of %d)", name, i+1, len(wf.Stages)),
		MaxAttempts: wf.MaxAttempts, Workflow: wf.ID, Stage: i,
	}
	task := func(name string, params map[string]string, inputs []domain.ArtifactRef) domain.TaskSpec {
		return domain.TaskSpec{Name: name, Capability: st.Type, Params: params, Inputs: inputs}
	}
	switch st.Mode {
	case domain.StagePerFile:
		for _, f := range files {
			spec.Tasks = append(spec.Tasks, task(path.Base(f.Name), copyParams(st.Params), []domain.ArtifactRef{f}))
		}
	case domain.StageParts:
		for p := 0; p < st.Parts; p++ {
			params := copyParams(st.Params)
			params["part"], params["parts"] = strconv.Itoa(p), strconv.Itoa(st.Parts)
			spec.Tasks = append(spec.Tasks, task(fmt.Sprintf("part %d", p+1), params, append([]domain.ArtifactRef(nil), files...)))
		}
	default:
		spec.Tasks = []domain.TaskSpec{task(title, copyParams(st.Params), append([]domain.ArtifactRef(nil), files...))}
	}
	if st.Combine != nil {
		spec.Reduce = &domain.TaskSpec{Name: stepTitle(*st.Combine), Capability: st.Combine.Type, Params: copyParams(st.Combine.Params)}
	}
	return spec
}

// stageJobs maps each workflow to its stages' jobs (the newest per
// stage, should there ever be two).
func stageJobs(jobs []domain.Job) map[domain.WorkflowID]map[int]domain.Job {
	out := map[domain.WorkflowID]map[int]domain.Job{}
	for _, j := range jobs { // oldest first: the newest wins
		if j.Workflow == "" {
			continue
		}
		if out[j.Workflow] == nil {
			out[j.Workflow] = map[int]domain.Job{}
		}
		out[j.Workflow][j.Stage] = j
	}
	return out
}

// currentStage is the furthest stage with a job (-1 before the first).
func currentStage(wf domain.Workflow, jobs map[int]domain.Job) int {
	cur := -1
	for i := range wf.Stages {
		if _, ok := jobs[i]; ok {
			cur = i
		}
	}
	return cur
}

// advanceWorkflows moves every running workflow forward: starts its
// first stage, starts the next stage once the current one completed,
// and records the outcome. A stage job still running for a workflow that
// has ended (canceled, possibly across a crash) is canceled: nothing
// waits for its results. It runs right after advanceJobs, so a stage
// whose job finished this pass hands on to the next in the same pass.
func (s *Server) advanceWorkflows(ctx context.Context) {
	wfs := s.workflows.list()
	if len(wfs) == 0 {
		return
	}
	jobs := s.jobs.list()
	for _, j := range jobs {
		if j.Workflow == "" || j.State != domain.JobRunning || s.workflows.running(j.Workflow) {
			continue
		}
		if _, known := s.workflows.get(j.Workflow); known {
			if err := s.CancelJob(ctx, j.ID); err != nil {
				log.Printf("manager: cancel job %s of ended workflow %s: %v", j.ID, j.Workflow, err)
			}
		}
	}
	stages := stageJobs(jobs)
	var attempts map[domain.JobID]map[string][]WorkloadRecord
	for _, wf := range wfs {
		if wf.State != domain.JobRunning {
			continue
		}
		cur := currentStage(wf, stages[wf.ID])
		if cur < 0 {
			s.startStage(ctx, wf, 0, wf.Inputs)
			continue
		}
		job := stages[wf.ID][cur]
		switch job.State {
		case domain.JobRunning:
		case domain.JobCompleted:
			if cur == len(wf.Stages)-1 {
				s.finishWorkflow(wf, domain.JobCompleted, "")
				continue
			}
			if attempts == nil {
				attempts = s.Workloads.byJob()
			}
			s.startStage(ctx, wf, cur+1, stageResults(job, attempts[job.ID]))
		case domain.JobCanceled:
			s.finishWorkflow(wf, domain.JobFailed, stageLabel(cur, wf.Stages[cur])+": its job was canceled")
		default:
			s.finishWorkflow(wf, domain.JobFailed, stageLabel(cur, wf.Stages[cur])+": "+job.Error)
		}
	}
}

// startStage submits stage i's job over files. A submission that can
// never succeed (the files don't fit the step, a type disabled since)
// fails the workflow with the reason; one with no device ready right now
// is retried next pass.
func (s *Server) startStage(ctx context.Context, wf domain.Workflow, i int, files []domain.ArtifactRef) {
	if !s.workflows.running(wf.ID) {
		return
	}
	label := stageLabel(i, wf.Stages[i])
	if i > 0 && len(files) == 0 {
		s.finishWorkflow(wf, domain.JobFailed, label+": the step before it made no files")
		return
	}
	job, err := s.SubmitJob(ctx, stageJob(wf, i, files))
	if errors.Is(err, ErrInvalidWorkload) || errors.Is(err, ErrPolicy) {
		s.finishWorkflow(wf, domain.JobFailed, label+": "+err.Error())
		return
	}
	if err != nil {
		s.workflows.setWaiting(wf.ID, err.Error())
		return
	}
	s.workflows.setWaiting(wf.ID, "")
	if !s.workflows.running(wf.ID) {
		// Canceled while its job was being submitted.
		if err := s.CancelJob(ctx, job.ID); err != nil {
			log.Printf("manager: cancel job %s of canceled workflow %s: %v", job.ID, wf.ID, err)
		}
		return
	}
	log.Printf("workflow.stage: %s step %d of %d (job %s)", wf.ID, i+1, len(wf.Stages), job.ID)
	s.publish(domain.EventWorkflowStage, "", map[string]any{"workflowId": string(wf.ID), "stage": i + 1, "jobId": string(job.ID)})
}

func (s *Server) finishWorkflow(wf domain.Workflow, state domain.JobState, reason string) {
	done, ok := s.workflows.finish(wf.ID, state, reason)
	if !ok {
		return
	}
	s.persistWorkflow(done)
	log.Printf("workflow.finished: %s %s %s", done.ID, done.State, reason)
	s.publish(domain.EventWorkflowFinished, "", map[string]any{"workflowId": string(done.ID), "state": string(done.State)})
}

func (s *Server) persistWorkflow(wf domain.Workflow) {
	if s.store == nil {
		return
	}
	if err := s.store.UpsertWorkflow(wf); err != nil {
		log.Printf("manager: persist workflow %s: %v", wf.ID, err)
	}
}

// CancelWorkflow stops a workflow: it is marked CANCELED first (so no
// further stage starts), then its running stage's job is canceled.
func (s *Server) CancelWorkflow(ctx context.Context, id domain.WorkflowID) error {
	if _, ok := s.workflows.get(id); !ok {
		return ErrUnknownWorkflow
	}
	wf, ok := s.workflows.finish(id, domain.JobCanceled, "canceled by the operator")
	if !ok {
		return nil // already finished
	}
	s.persistWorkflow(wf)
	s.publish(domain.EventWorkflowFinished, "", map[string]any{"workflowId": string(id), "state": string(wf.State)})
	for _, j := range s.jobs.list() {
		if j.Workflow == id && j.State == domain.JobRunning {
			if err := s.CancelJob(ctx, j.ID); err != nil {
				log.Printf("manager: cancel job %s of workflow %s: %v", j.ID, id, err)
			}
		}
	}
	return nil
}

// workflowLiveArtifacts adds what running workflows still need to live:
// their own files and every result of their stages so far — between one
// stage's job finishing and the next one's starting, nothing else holds
// the next stage's inputs.
func (s *Server) workflowLiveArtifacts(live map[string]bool) {
	var running []domain.Workflow
	for _, wf := range s.workflows.list() {
		if wf.State == domain.JobRunning {
			running = append(running, wf)
		}
	}
	if len(running) == 0 {
		return
	}
	attempts := s.Workloads.byJob()
	stages := stageJobs(s.jobs.list())
	for _, wf := range running {
		for _, in := range wf.Inputs {
			live[in.SHA256] = true
		}
		for _, j := range stages[wf.ID] {
			for _, as := range attempts[j.ID] {
				for _, a := range as {
					for _, o := range a.Status.Outputs {
						live[o.SHA256] = true
					}
				}
			}
		}
	}
}
