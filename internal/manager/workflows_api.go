package manager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"home-harness/internal/domain"
)

// Workflow API (workflows.go). Operator credentials, like every route
// outside /v1.

type workflowRequest struct {
	Name   string `json:"name,omitempty"`
	Inputs []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"inputs,omitempty"`
	Stages      []domain.WorkflowStage `json:"stages"`
	MaxAttempts int                    `json:"maxAttempts,omitempty"`
}

func writeWorkflowError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownWorkflow):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrInvalidWorkload):
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrPolicy):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrNodeNotConnected), errors.Is(err, ErrNoReadyNode), errors.Is(err, ErrNoEligibleNode):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// apiPostWorkflow is POST /workflows {name, inputs: [{name, sha256}],
// stages: [{type, params, mode, parts, combine: {type, params}}], maxAttempts}.
func (s *Server) apiPostWorkflow(w http.ResponseWriter, r *http.Request) {
	var req workflowRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	// A misspelled field in a hand-written workflow would otherwise be
	// dropped without a word (a step's parameters, its mode).
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	spec := WorkflowSpec{Name: req.Name, Stages: req.Stages, MaxAttempts: req.MaxAttempts}
	for _, in := range req.Inputs {
		spec.Inputs = append(spec.Inputs, domain.ArtifactRef{Name: in.Name, SHA256: in.SHA256})
	}
	wf, err := s.SubmitWorkflow(r.Context(), spec)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "workflow.submitted", "", actorFrom(r.Context()), map[string]any{
		"workflowId": string(wf.ID), "steps": len(wf.Stages), "files": len(wf.Inputs), "maxAttempts": wf.MaxAttempts,
	})
	writeJSON(w, http.StatusAccepted, s.toWorkflowView(wf))
}

func (s *Server) apiListWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs := s.workflows.list()
	out := make([]workflowView, 0, len(wfs))
	stages, attempts := stageJobs(s.jobs.list()), s.Workloads.byJob() // one scan for the whole list
	for _, wf := range wfs {
		out = append(out, s.workflowView(wf, stages[wf.ID], attempts))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiGetWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.workflows.get(domain.WorkflowID(r.PathValue("id")))
	if !ok {
		writeWorkflowError(w, ErrUnknownWorkflow)
		return
	}
	writeJSON(w, http.StatusOK, s.toWorkflowView(wf))
}

func (s *Server) apiCancelWorkflow(w http.ResponseWriter, r *http.Request) {
	id := domain.WorkflowID(r.PathValue("id"))
	if err := s.CancelWorkflow(r.Context(), id); err != nil {
		writeWorkflowError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "workflow.canceled", "", actorFrom(r.Context()), map[string]any{"workflowId": string(id)})
	wf, _ := s.workflows.get(id)
	writeJSON(w, http.StatusAccepted, s.toWorkflowView(wf))
}

type workflowStepView struct {
	Type   domain.CapabilityName `json:"type"`
	Title  string                `json:"title"`
	Params map[string]string     `json:"params,omitempty"`
}

// Stage states besides its job's (RUNNING, COMPLETED, FAILED, CANCELED).
const (
	stagePending = "PENDING" // the stages before it haven't finished
	stageWaiting = "WAITING" // its turn: its job isn't submitted yet
	stageSkipped = "SKIPPED" // the workflow ended before it
)

type workflowStageView struct {
	Stage int    `json:"stage"` // from 1
	Name  string `json:"name,omitempty"`
	workflowStepView
	Mode    string            `json:"mode"`
	Parts   int               `json:"parts,omitempty"`
	Combine *workflowStepView `json:"combine,omitempty"`
	State   string            `json:"state"`
	Job     domain.JobID      `json:"job,omitempty"`
	Counts  *jobCounts        `json:"counts,omitempty"`
	Error   string            `json:"error,omitempty"`
	Waiting string            `json:"waiting,omitempty"`
	// Outputs are what a completed stage handed on: the next stage's
	// files, or the workflow's results for the last.
	Outputs []domain.ArtifactRef `json:"outputs,omitempty"`
}

type workflowView struct {
	ID          domain.WorkflowID    `json:"id"`
	Name        string               `json:"name,omitempty"`
	State       domain.JobState      `json:"state"`
	Error       string               `json:"error,omitempty"`
	MaxAttempts int                  `json:"maxAttempts"`
	CreatedAt   time.Time            `json:"createdAt"`
	FinishedAt  time.Time            `json:"finishedAt,omitempty"`
	Inputs      []domain.ArtifactRef `json:"inputs,omitempty"`
	// Stage is how far it got: the stage now running or last run (from
	// 1), 0 before the first stage's job.
	Stage  int                 `json:"stage"`
	Stages []workflowStageView `json:"stages"`
	// Outputs are the workflow's results: the last stage's, once done.
	Outputs []domain.ArtifactRef `json:"outputs,omitempty"`
}

func (s *Server) toWorkflowView(wf domain.Workflow) workflowView {
	return s.workflowView(wf, stageJobs(s.jobs.list())[wf.ID], s.Workloads.byJob())
}

func stepView(step domain.WorkflowStep) workflowStepView {
	return workflowStepView{Type: step.Type, Title: stepTitle(step), Params: step.Params}
}

func (s *Server) workflowView(wf domain.Workflow, jobs map[int]domain.Job, attempts map[domain.JobID]map[string][]WorkloadRecord) workflowView {
	v := workflowView{ID: wf.ID, Name: wf.Name, State: wf.State, Error: wf.Error, MaxAttempts: wf.MaxAttempts,
		CreatedAt: wf.CreatedAt, FinishedAt: wf.FinishedAt, Inputs: wf.Inputs}
	cur := currentStage(wf, jobs)
	v.Stage = cur + 1
	for i, st := range wf.Stages {
		sv := workflowStageView{Stage: i + 1, Name: st.Name, workflowStepView: stepView(st.WorkflowStep), Mode: st.Mode, Parts: st.Parts}
		if st.Combine != nil {
			c := stepView(*st.Combine)
			sv.Combine = &c
		}
		job, started := jobs[i]
		switch {
		case started:
			jv := s.jobView(job, attempts[job.ID], false)
			sv.State, sv.Job, sv.Counts, sv.Error = string(job.State), job.ID, &jv.Counts, job.Error
			if job.State == domain.JobCompleted {
				sv.Outputs = stageResults(job, attempts[job.ID])
			}
		case wf.State != domain.JobRunning:
			sv.State = stageSkipped
		case i == 0 || (i == cur+1 && jobs[cur].State == domain.JobCompleted):
			sv.State, sv.Waiting = stageWaiting, s.workflows.waitingReason(wf.ID)
		default:
			sv.State = stagePending
		}
		v.Stages = append(v.Stages, sv)
	}
	if wf.State == domain.JobCompleted && len(v.Stages) > 0 {
		v.Outputs = v.Stages[len(v.Stages)-1].Outputs
	}
	return v
}
