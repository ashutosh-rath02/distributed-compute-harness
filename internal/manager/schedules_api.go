package manager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"home-harness/internal/domain"
)

// Schedule API (schedules.go). Operator credentials like every route
// outside /v1; creating, pausing, resuming, running and deleting are
// POST/DELETE, never GET.

func (s *Server) registerScheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /schedules", s.apiPostSchedule)
	mux.HandleFunc("GET /schedules", s.apiListSchedules)
	mux.HandleFunc("GET /schedules/{id}", s.apiGetSchedule)
	mux.HandleFunc("POST /schedules/{id}/pause", s.apiScheduleAction("schedule.paused", s.PauseSchedule))
	mux.HandleFunc("POST /schedules/{id}/resume", s.apiScheduleAction("schedule.resumed", s.ResumeSchedule))
	mux.HandleFunc("POST /schedules/{id}/run", s.apiRunSchedule)
	mux.HandleFunc("DELETE /schedules/{id}", s.apiDeleteSchedule)
}

type scheduleRequest struct {
	Name     string           `json:"name,omitempty"`
	Schedule string           `json:"schedule"`
	Timezone string           `json:"timezone,omitempty"`
	Missed   string           `json:"missed,omitempty"`
	Overlap  string           `json:"overlap,omitempty"`
	MaxRuns  int              `json:"maxRuns,omitempty"`
	EndAt    *time.Time       `json:"endAt,omitempty"`
	Job      *jobRequest      `json:"job,omitempty"`
	Workflow *workflowRequest `json:"workflow,omitempty"`
}

func writeScheduleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownSchedule):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrScheduleState):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		writeWorkflowError(w, err) // invalid → 400, policy → 403, no device → 409
	}
}

func (s *Server) apiPostSchedule(w http.ResponseWriter, r *http.Request) {
	var req scheduleRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	in := ScheduleRequest{Name: req.Name, Spec: req.Schedule, Timezone: req.Timezone, Missed: req.Missed, Overlap: req.Overlap, MaxRuns: req.MaxRuns}
	if req.EndAt != nil {
		in.EndAt = *req.EndAt
	}
	if req.Job != nil {
		t := &domain.JobTemplate{Name: req.Job.Name, MaxAttempts: req.Job.MaxAttempts, Priority: domain.Priority(req.Job.Priority)}
		for _, tk := range req.Job.Tasks {
			t.Tasks = append(t.Tasks, tk.spec())
		}
		if req.Job.Reduce != nil {
			rs := req.Job.Reduce.spec()
			t.Reduce = &rs
		}
		in.Job = t
	}
	if req.Workflow != nil {
		t := &domain.WorkflowTemplate{Name: req.Workflow.Name, Stages: req.Workflow.Stages, MaxAttempts: req.Workflow.MaxAttempts}
		for _, f := range req.Workflow.Inputs {
			t.Inputs = append(t.Inputs, domain.ArtifactRef{Name: f.Name, SHA256: f.SHA256})
		}
		in.Workflow = t
	}
	sc, err := s.CreateSchedule(r.Context(), in)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "schedule.created", "", actorFrom(r.Context()), map[string]any{
		"scheduleId": string(sc.ID), "name": sc.Name, "schedule": sc.Spec, "timezone": sc.Timezone, "kind": sc.Kind(),
		"missed": sc.Missed, "overlap": sc.Overlap, "maxRuns": sc.MaxRuns,
	})
	writeJSON(w, http.StatusCreated, s.scheduleView(sc, true))
}

func (s *Server) apiListSchedules(w http.ResponseWriter, r *http.Request) {
	out := []scheduleView{}
	if t := s.scheduleTbl(); t != nil {
		for _, sc := range t.list() {
			out = append(out, s.scheduleView(sc, false))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiGetSchedule(w http.ResponseWriter, r *http.Request) {
	t := s.scheduleTbl()
	if t == nil {
		writeScheduleError(w, ErrUnknownSchedule)
		return
	}
	sc, ok := t.get(domain.ScheduleID(r.PathValue("id")))
	if !ok {
		writeScheduleError(w, ErrUnknownSchedule)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleView(sc, true))
}

func (s *Server) apiScheduleAction(kind string, do func(domain.ScheduleID) (domain.Schedule, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := domain.ScheduleID(r.PathValue("id"))
		sc, err := do(id)
		if err != nil {
			writeScheduleError(w, err)
			return
		}
		s.audit(domain.AuditSecurity, kind, "", actorFrom(r.Context()), map[string]any{"scheduleId": string(id), "name": sc.Name})
		writeJSON(w, http.StatusOK, s.scheduleView(sc, false))
	}
}

func (s *Server) apiRunSchedule(w http.ResponseWriter, r *http.Request) {
	run, err := s.RunScheduleNow(r.Context(), domain.ScheduleID(r.PathValue("id")))
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	status := http.StatusAccepted
	if run.Outcome == domain.RunFailed {
		status = http.StatusConflict // it could not be started; the reason is in the run
	}
	writeJSON(w, status, s.runView(run))
}

func (s *Server) apiDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := domain.ScheduleID(r.PathValue("id"))
	name := ""
	if t := s.scheduleTbl(); t != nil {
		if sc, ok := t.get(id); ok {
			name = sc.Name
		}
	}
	if err := s.DeleteSchedule(id); err != nil {
		writeScheduleError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "schedule.deleted", "", actorFrom(r.Context()), map[string]any{"scheduleId": string(id), "name": name})
	w.WriteHeader(http.StatusNoContent)
}

type runView struct {
	domain.ScheduleRun
	// State is how the job or workflow it started stands now.
	State string `json:"state,omitempty"`
}

type scheduleView struct {
	ID         domain.ScheduleID    `json:"id"`
	Name       string               `json:"name,omitempty"`
	Schedule   string               `json:"schedule"`
	Timezone   string               `json:"timezone"`
	Kind       string               `json:"kind"`
	Summary    string               `json:"summary"`
	State      domain.ScheduleState `json:"state"`
	Missed     string               `json:"missed"`
	Overlap    string               `json:"overlap"`
	MaxRuns    int                  `json:"maxRuns,omitempty"`
	EndAt      *time.Time           `json:"endAt,omitempty"`
	Runs       int                  `json:"runs"`
	CreatedAt  time.Time            `json:"createdAt"`
	NextRun    *time.Time           `json:"nextRun,omitempty"`
	LastRun    *runView             `json:"lastRun,omitempty"`
	QueuedSlot *time.Time           `json:"queuedSlot,omitempty"`
	History    []runView            `json:"history,omitempty"`
}

func (s *Server) runView(r domain.ScheduleRun) runView {
	v := runView{ScheduleRun: r}
	switch {
	case r.Job != "":
		if j, ok := s.jobs.get(r.Job); ok {
			v.State = string(j.State)
		}
	case r.Workflow != "":
		if wf, ok := s.workflows.get(r.Workflow); ok {
			v.State = string(wf.State)
		}
	}
	return v
}

// scheduleView: history (newest first) only when asked for.
func (s *Server) scheduleView(sc domain.Schedule, withHistory bool) scheduleView {
	v := scheduleView{ID: sc.ID, Name: sc.Name, Schedule: sc.Spec, Timezone: sc.Timezone, Kind: sc.Kind(), State: sc.State,
		Missed: sc.Missed, Overlap: sc.Overlap, MaxRuns: sc.MaxRuns, Runs: sc.Runs, CreatedAt: sc.CreatedAt}
	if sc.Workflow != nil {
		v.Summary = workflowTitle(sc.Workflow.Stages)
	} else if sc.Job != nil {
		v.Summary = jobSummary(*sc.Job)
	}
	if !sc.EndAt.IsZero() {
		e := sc.EndAt
		v.EndAt = &e
	}
	if !sc.QueuedSlot.IsZero() {
		q := sc.QueuedSlot
		v.QueuedSlot = &q
	}
	if sc.State == domain.ScheduleActive {
		if spec, err := scheduleSpec(sc); err == nil {
			if n, ok := spec.Next(sc.LastSlot); ok && (sc.EndAt.IsZero() || !n.After(sc.EndAt)) {
				v.NextRun = &n
			}
		}
	}
	if n := len(sc.History); n > 0 {
		last := s.runView(sc.History[n-1])
		v.LastRun = &last
		if withHistory {
			for i := n - 1; i >= 0; i-- {
				v.History = append(v.History, s.runView(sc.History[i]))
			}
		}
	}
	return v
}

func jobSummary(j domain.JobTemplate) string {
	if len(j.Tasks) == 0 {
		return "job"
	}
	what := string(j.Tasks[0].Capability)
	if what == "" {
		what = "command"
	}
	out := what
	if len(j.Tasks) > 1 {
		out += " x" + strconv.Itoa(len(j.Tasks))
	}
	if j.Reduce != nil {
		out += " + combine"
	}
	return out
}
