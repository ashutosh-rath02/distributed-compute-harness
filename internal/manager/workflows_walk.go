package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"home-harness/internal/domain"
)

// Conditional and failure-aware stages. Where a workflow stands is still
// derived, never stored: walkWorkflow reads the stages' jobs in order and
// decides, from them alone, which stages ran, which were skipped (their
// "when" was false) and what happens next. Because a skip is a pure
// function of the stages before it, a manager restarted at any moment
// reaches the same decisions, and a stage that has a job is never started
// again. The only stored extra is domain.StageMark, for a tolerated
// failure that never got as far as a job.
//
// What a stage is given: the results of the nearest earlier stage that
// COMPLETED, or the workflow's own files when none did. A skipped or
// failed-and-continued stage is passed over, so it never breaks the
// typing of the stages after it. checkWorkflow type-checks a stage
// against every set of files it could be given (inputCandidates).

const (
	maxStageTimeoutSeconds = 24 * 3600
	maxStageRetries        = 5
	maxInputCandidates     = 32
)

// onFailure modes.
const (
	onFailStop     = "stop"
	onFailContinue = "continue"
	onFailRetry    = "retry"
)

// parseOnFailure reads a stage's onFailure: "" or "stop", "continue", or
// "retry:N" (N from 1 to 5, whole-stage re-runs after the stage's tasks
// ran out of attempts).
func parseOnFailure(s string) (mode string, retries int, err error) {
	switch {
	case s == "" || s == onFailStop:
		return onFailStop, 0, nil
	case s == onFailContinue:
		return onFailContinue, 0, nil
	case strings.HasPrefix(s, "retry:"):
		n, convErr := strconv.Atoi(strings.TrimPrefix(s, "retry:"))
		if convErr != nil || n < 1 || n > maxStageRetries {
			return "", 0, fmt.Errorf("%w: onFailure %q: retry:N takes N from 1 to %d", ErrInvalidWorkload, s, maxStageRetries)
		}
		return onFailRetry, n, nil
	}
	return "", 0, fmt.Errorf("%w: onFailure %q isn't stop, continue or retry:N", ErrInvalidWorkload, s)
}

// optionalStage: a stage that may hand on nothing (skipped by its
// condition, or failing without stopping the workflow).
func optionalStage(st domain.WorkflowStage) bool {
	mode, _, _ := parseOnFailure(st.OnFailure)
	return st.When != "" || mode == onFailContinue
}

// inputCandidate is one set of files stage i might be given; from is the
// stage that makes them, or -1 for the workflow's own files.
type inputCandidate struct {
	files []domain.ArtifactRef
	from  int
}

// inputCandidates lists every set of files stage i can be given, nearest
// first: the possible results of the stage before it, and, when that one
// is optional, of the one before that, and so on to the first stage that
// always completes (or the workflow's own files).
func inputCandidates(stages []domain.WorkflowStage, outs [][][]domain.ArtifactRef, inputs []domain.ArtifactRef, i int) []inputCandidate {
	var out []inputCandidate
	for j := i - 1; j >= 0; j-- {
		for _, o := range outs[j] {
			out = append(out, inputCandidate{files: o, from: j})
		}
		if !optionalStage(stages[j]) {
			return out
		}
	}
	return append(out, inputCandidate{files: inputs, from: -1})
}

// stageJobCounts counts the jobs each workflow stage has had (one, plus
// one per retry).
func stageJobCounts(jobs []domain.Job) map[domain.WorkflowID]map[int]int {
	out := map[domain.WorkflowID]map[int]int{}
	for _, j := range jobs {
		if j.Workflow == "" {
			continue
		}
		if out[j.Workflow] == nil {
			out[j.Workflow] = map[int]int{}
		}
		out[j.Workflow][j.Stage]++
	}
	return out
}

// Stage states a walk reports (besides the job states RUNNING, COMPLETED,
// FAILED, CANCELED).
type stageInfo struct {
	state    string
	reason   string // why skipped, or what failed
	job      domain.Job
	hasJob   bool
	outputs  []domain.ArtifactRef // when COMPLETED
	retrying string               // "run 2 of 3" while a failed stage is being run again
}

const (
	actNone = iota
	actStart
	actRetry
	actFinish
)

type walkAction struct {
	kind  int
	stage int
	files []domain.ArtifactRef
	state domain.JobState
	text  string
}

func condEnv(infos []stageInfo, inputs []domain.ArtifactRef) CondEnv {
	env := CondEnv{Inputs: refNameList(inputs)}
	for _, in := range infos {
		st := CondStage{State: in.state}
		if in.state == condCompleted {
			st.Outputs = refNameList(in.outputs)
		}
		env.Stages = append(env.Stages, st)
	}
	return env
}

func refNameList(files []domain.ArtifactRef) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}

// provider is what the stage after infos is given.
func provider(infos []stageInfo, inputs []domain.ArtifactRef) []domain.ArtifactRef {
	for j := len(infos) - 1; j >= 0; j-- {
		if infos[j].state == condCompleted {
			return append([]domain.ArtifactRef(nil), infos[j].outputs...)
		}
	}
	return append([]domain.ArtifactRef(nil), inputs...)
}

// walkWorkflow derives every stage's state from the stages' jobs (newest
// per stage) and says what the manager should do next. results reads a
// completed stage's results. It changes nothing, so the API calls it too.
func walkWorkflow(wf domain.Workflow, jobs map[int]domain.Job, counts map[int]int, results func(domain.Job) []domain.ArtifactRef) ([]stageInfo, walkAction) {
	running := wf.State == domain.JobRunning
	infos := make([]stageInfo, len(wf.Stages))
	marks := map[int]string{}
	for _, m := range wf.Marks {
		marks[m.Stage] = m.Reason
	}
	act := walkAction{kind: actNone}
	stopped := false // a stage ended the workflow: nothing after it runs
	fill := func(from int) {
		for k := from; k < len(infos); k++ {
			if running && !stopped {
				infos[k].state = stagePending
			} else {
				infos[k].state, infos[k].reason = stageSkipped, "the workflow ended before it ran"
			}
		}
	}
	for i, st := range wf.Stages {
		in := &infos[i]
		label := stageLabel(i, st)
		if stopped {
			fill(i)
			return infos, act
		}
		if reason, ok := marks[i]; ok {
			in.state, in.reason = condFailed, reason
			continue
		}
		job, started := jobs[i]
		if !started {
			if st.When != "" {
				cond, err := ParseCondition(st.When, i)
				if err != nil { // can't happen: checked at submit
					if running {
						act = walkAction{kind: actFinish, state: domain.JobFailed, text: label + ": when: " + err.Error()}
					}
					stopped = true
					fill(i)
					return infos, act
				}
				if !cond.Eval(condEnv(infos[:i], wf.Inputs)) {
					in.state, in.reason = condSkipped, "condition not met: "+cond.String()
					continue
				}
			}
			if !running {
				stopped = true
				fill(i)
				return infos, act
			}
			in.state = stageWaiting
			act = walkAction{kind: actStart, stage: i, files: provider(infos[:i], wf.Inputs)}
			fill(i + 1)
			return infos, act
		}
		in.job, in.hasJob = job, true
		switch job.State {
		case domain.JobRunning:
			in.state = string(domain.JobRunning)
			fill(i + 1)
			return infos, act
		case domain.JobCompleted:
			in.state = condCompleted
			in.outputs = results(job)
		case domain.JobCanceled:
			in.state, in.reason = string(domain.JobCanceled), job.Error
			if running {
				act = walkAction{kind: actFinish, state: domain.JobFailed, text: label + ": its job was canceled"}
			}
			stopped = true
		default: // FAILED
			in.state, in.reason = condFailed, job.Error
			mode, retries, _ := parseOnFailure(st.OnFailure)
			done := counts[i] - 1
			switch {
			case running && mode == onFailRetry && done < retries:
				in.retrying = fmt.Sprintf("run %d of %d", done+2, retries+1)
				act = walkAction{kind: actRetry, stage: i, files: provider(infos[:i], wf.Inputs)}
				fill(i + 1)
				return infos, act
			case mode == onFailContinue:
			default:
				if running {
					act = walkAction{kind: actFinish, state: domain.JobFailed, text: label + ": " + job.Error}
				}
				stopped = true
			}
		}
	}
	if running && !stopped {
		act = walkAction{kind: actFinish, state: domain.JobCompleted}
	}
	return infos, act
}

// advanceWorkflows moves every running workflow forward by what
// walkWorkflow says: starts a stage (or runs a failed one again), or
// records the outcome. A stage job still running for a workflow that has
// ended (canceled, possibly across a crash) is canceled: nothing waits
// for its results. It runs right after advanceJobs, so a stage whose job
// finished this pass hands on to the next in the same pass.
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
	stages, counts := stageJobs(jobs), stageJobCounts(jobs)
	var attempts map[domain.JobID]map[string][]WorkloadRecord
	results := func(job domain.Job) []domain.ArtifactRef {
		if attempts == nil {
			attempts = s.Workloads.byJob()
		}
		return stageResults(job, attempts[job.ID])
	}
	for _, wf := range wfs {
		if wf.State != domain.JobRunning {
			continue
		}
		_, act := walkWorkflow(wf, stages[wf.ID], counts[wf.ID], results)
		switch act.kind {
		case actStart, actRetry:
			s.startStage(ctx, wf, act.stage, act.files, act.kind == actRetry)
		case actFinish:
			s.finishWorkflow(wf, act.state, act.text)
		}
	}
}

// startStage submits stage i's job over files. A submission that can
// never succeed (the files don't fit the step, a type disabled since)
// fails the workflow with the reason, unless the stage tolerates failure
// (onFailure continue: the failure is recorded and the workflow moves
// on); one with no device ready right now is retried next pass.
func (s *Server) startStage(ctx context.Context, wf domain.Workflow, i int, files []domain.ArtifactRef, again bool) {
	if !s.workflows.running(wf.ID) {
		return
	}
	label := stageLabel(i, wf.Stages[i])
	fail := func(reason string) {
		if mode, _, _ := parseOnFailure(wf.Stages[i].OnFailure); mode == onFailContinue && !again {
			if updated, ok := s.workflows.addMark(wf.ID, domain.StageMark{Stage: i, Reason: reason}); ok {
				s.persistWorkflow(updated)
				log.Printf("workflow.stage: %s step %d of %d failed to start, continuing: %s", wf.ID, i+1, len(wf.Stages), reason)
				s.kickDispatch()
			}
			return
		}
		s.finishWorkflow(wf, domain.JobFailed, label+": "+reason)
	}
	if i > 0 && len(files) == 0 {
		fail("nothing to work on: no step before it made files")
		return
	}
	job, err := s.SubmitJob(ctx, stageJob(wf, i, files))
	if errors.Is(err, ErrInvalidWorkload) || errors.Is(err, ErrPolicy) {
		fail(err.Error())
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

// addMark records a tolerated failure that has no job; false if the
// workflow isn't running or already has a mark for the stage.
func (t *workflowTable) addMark(id domain.WorkflowID, m domain.StageMark) (domain.Workflow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	wf, ok := t.workflows[id]
	if !ok || wf.State != domain.JobRunning {
		return domain.Workflow{}, false
	}
	for _, have := range wf.Marks {
		if have.Stage == m.Stage {
			return domain.Workflow{}, false
		}
	}
	wf.Marks = append(wf.Marks, m)
	return *wf, true
}
