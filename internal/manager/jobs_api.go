package manager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"home-harness/internal/domain"
)

// Batch job API (jobs.go).

type jobTaskRequest struct {
	Name       string            `json:"name,omitempty"`
	Target     string            `json:"target,omitempty"`
	Capability string            `json:"capability,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	Inputs     []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"inputs,omitempty"`
	Outputs      []string                    `json:"outputs,omitempty"`
	Requirements domain.ResourceRequirements `json:"requirements,omitempty"`
}

func (t jobTaskRequest) spec() domain.TaskSpec {
	ts := domain.TaskSpec{Name: t.Name, Target: domain.NodeID(t.Target), Capability: domain.CapabilityName(t.Capability), Params: t.Params, Command: t.Command, Args: t.Args, Outputs: t.Outputs, Requirements: t.Requirements}
	for _, in := range t.Inputs {
		ts.Inputs = append(ts.Inputs, domain.ArtifactRef{Name: in.Name, SHA256: in.SHA256})
	}
	return ts
}

type jobRequest struct {
	Name        string           `json:"name,omitempty"`
	Tasks       []jobTaskRequest `json:"tasks"`
	Reduce      *jobTaskRequest  `json:"reduce,omitempty"`
	MaxAttempts int              `json:"maxAttempts,omitempty"`
}

func (s *Server) apiPostJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	// Larger than a single workload's cap: a 1000-task job with args.
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	spec := JobSpec{Name: req.Name, MaxAttempts: req.MaxAttempts}
	for _, t := range req.Tasks {
		spec.Tasks = append(spec.Tasks, t.spec())
	}
	if req.Reduce != nil {
		rs := req.Reduce.spec()
		spec.Reduce = &rs
	}
	job, err := s.SubmitJob(r.Context(), spec)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidWorkload):
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrPolicy):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrNodeNotConnected), errors.Is(err, ErrNoReadyNode), errors.Is(err, ErrNoEligibleNode):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	s.audit(domain.AuditSecurity, "job.submitted", "", actorFrom(r.Context()), map[string]any{
		"jobId": string(job.ID), "tasks": len(job.Tasks), "reduce": job.Reduce != nil, "maxAttempts": job.MaxAttempts,
	})
	writeJSON(w, http.StatusAccepted, s.toJobView(job, false))
}

func (s *Server) apiListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.jobs.list()
	out := make([]jobView, 0, len(jobs))
	all := s.Workloads.byJob() // one scan for the whole list
	for _, j := range jobs {
		out = append(out, s.jobView(j, all[j.ID], false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.get(domain.JobID(r.PathValue("id")))
	if !ok {
		http.Error(w, ErrUnknownJob.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.toJobView(job, true))
}

func (s *Server) apiCancelJob(w http.ResponseWriter, r *http.Request) {
	id := domain.JobID(r.PathValue("id"))
	if err := s.CancelJob(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.audit(domain.AuditSecurity, "job.canceled", "", actorFrom(r.Context()), map[string]any{"jobId": string(id)})
	job, _ := s.jobs.get(id)
	writeJSON(w, http.StatusAccepted, s.toJobView(job, false))
}

// jobCounts summarizes task progress.
type jobCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Active    int `json:"active"`
	Waiting   int `json:"waiting"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
}

type jobTaskView struct {
	Key      string               `json:"key"`
	Name     string               `json:"name,omitempty"`
	State    string               `json:"state"`
	Attempts int                  `json:"attempts"`
	Workload domain.WorkloadID    `json:"workload,omitempty"`
	Node     domain.NodeID        `json:"node,omitempty"`
	NodeName string               `json:"nodeName,omitempty"` // the operator's alias, else the agent's name
	Outputs  []domain.ArtifactRef `json:"outputs,omitempty"`
	Error    string               `json:"error,omitempty"`
	Waiting  string               `json:"waiting,omitempty"`
}

type jobView struct {
	ID          domain.JobID    `json:"id"`
	Name        string          `json:"name,omitempty"`
	State       domain.JobState `json:"state"`
	Error       string          `json:"error,omitempty"`
	MaxAttempts int             `json:"maxAttempts"`
	CreatedAt   time.Time       `json:"createdAt"`
	FinishedAt  time.Time       `json:"finishedAt,omitempty"`
	Counts      jobCounts       `json:"counts"`
	Tasks       []jobTaskView   `json:"tasks,omitempty"`
	Reduce      *jobTaskView    `json:"reduce,omitempty"`
	// Outputs is the job's result: the reduce's outputs when it has one.
	Outputs []domain.ArtifactRef `json:"outputs,omitempty"`
}

func (s *Server) toJobView(job domain.Job, withTasks bool) jobView {
	return s.jobView(job, s.Workloads.byJob()[job.ID], withTasks)
}

func (s *Server) nodeDisplayName(id domain.NodeID) string {
	if id == "" {
		return ""
	}
	if alias := s.meta.get(id).Alias; alias != "" {
		return alias
	}
	if rec, ok := s.Registry.Get(id); ok {
		return rec.Node.Name
	}
	return ""
}

func (s *Server) jobView(job domain.Job, byTask map[string][]WorkloadRecord, withTasks bool) jobView {
	v := jobView{ID: job.ID, Name: job.Name, State: job.State, Error: job.Error, MaxAttempts: job.MaxAttempts, CreatedAt: job.CreatedAt, FinishedAt: job.FinishedAt}
	view := func(key, name string) jobTaskView {
		tp := deriveTask(byTask[key], job.MaxAttempts)
		tv := jobTaskView{Key: key, Name: name, State: tp.state, Attempts: tp.attempts, Outputs: tp.outputs, Error: tp.err}
		if tp.current != nil {
			tv.Workload, tv.Node = tp.current.Workload.ID, tp.current.Workload.Target
			tv.NodeName = s.nodeDisplayName(tv.Node)
		}
		if tp.state == taskWaiting && job.State == domain.JobRunning {
			tv.Waiting = s.jobs.waitingReason(job.ID, key)
		}
		return tv
	}
	v.Counts.Total = len(job.Tasks)
	for i, t := range job.Tasks {
		tv := view(domain.TaskKey(i), t.Name)
		switch tv.State {
		case taskDone:
			v.Counts.Completed++
		case taskActive:
			v.Counts.Active++
		case taskWaiting:
			v.Counts.Waiting++
		case taskFailed:
			v.Counts.Failed++
		case taskCanceled:
			v.Counts.Canceled++
		}
		if withTasks {
			v.Tasks = append(v.Tasks, tv)
		}
	}
	if job.Reduce != nil {
		rv := view(domain.ReduceTask, job.Reduce.Name)
		if len(byTask[domain.ReduceTask]) == 0 && job.State == domain.JobRunning {
			rv.State = "PENDING_TASKS"
		}
		v.Reduce = &rv
		v.Outputs = rv.Outputs
	}
	return v
}
