package manager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"home-harness/internal/domain"
)

// AI planner API (planner.go). Operator credentials only, like every
// route outside /v1: the AI key can't plan, and above all can't approve.

type planView struct {
	Plan
	// ChatState is the planning chat's state while planning (QUEUED:
	// the model's device is busy).
	ChatState domain.WorkloadState `json:"chatState,omitempty"`
}

func (s *Server) toPlanView(p Plan) planView {
	v := planView{Plan: p}
	if p.State == PlanPlanning && p.WorkloadID != "" {
		if rec, ok := s.Workloads.Get(p.WorkloadID); ok {
			v.ChatState = rec.Status.State
		}
	}
	return v
}

func writePlanError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownPlan):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrInvalidWorkload):
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrPolicy):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrPlanState):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrPlanBusy):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, ErrPlanUnavailable), errors.Is(err, ErrNodeNotConnected), errors.Is(err, ErrNoReadyNode), errors.Is(err, ErrNoEligibleNode):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// apiPostPlan is POST /plans {request, model, files: [{name, sha256}]}.
func (s *Server) apiPostPlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := s.StartPlan(r.Context(), req, actorFrom(r.Context()))
	if err != nil {
		writePlanError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.toPlanView(p))
}

// apiListPlans is GET /plans: every plan, newest first, and the models
// that can plan right now.
func (s *Server) apiListPlans(w http.ResponseWriter, r *http.Request) {
	plans := s.plans.list()
	views := make([]planView, 0, len(plans))
	for _, p := range plans {
		views = append(views, s.toPlanView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": views, "models": sortedKeys(s.planModels())})
}

func (s *Server) apiGetPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := s.plans.get(r.PathValue("id"))
	if !ok {
		writePlanError(w, ErrUnknownPlan)
		return
	}
	writeJSON(w, http.StatusOK, s.toPlanView(p))
}

func (s *Server) apiApprovePlan(w http.ResponseWriter, r *http.Request) {
	p, err := s.ApprovePlan(r.Context(), r.PathValue("id"), actorFrom(r.Context()))
	if err != nil {
		writePlanError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.toPlanView(p))
}

func (s *Server) apiRejectPlan(w http.ResponseWriter, r *http.Request) {
	p, err := s.RejectPlan(r.PathValue("id"), actorFrom(r.Context()))
	if err != nil {
		writePlanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toPlanView(p))
}
