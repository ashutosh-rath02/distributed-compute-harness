package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Follow-up steps in a plan (roadmap item 15): "then" turns a plan into
// a workflow — the plan's task (and its combine) is the first stage, each
// follow-up a stage on the results of the one before. A plan without
// follow-ups is the one-step job it always was. Every step gets the same
// deterministic checks: a type the planner may use, its parameters, and
// the whole workflow checked as SubmitWorkflow checks it.

// PlanThen is a validated follow-up step and how it runs over the
// results of the step before: once with all of them, or once per file.
type PlanThen struct {
	PlanStep
	Mode string `json:"mode"`
	// Runs is how many times it will run, once the plan is checked.
	Runs int `json:"runs,omitempty"`
}

type proposedThen struct {
	Type   string                     `json:"type"`
	Params map[string]json.RawMessage `json:"params"`
	Mode   string                     `json:"mode"`
}

// draftThen checks a proposal's follow-up steps against the types it was
// offered.
func draftThen(then []proposedThen, allowed map[domain.CapabilityName]catalog.Type) ([]PlanThen, error) {
	if len(then) > maxPlanSteps-1 {
		return nil, fmt.Errorf("the plan has %d steps; at most %d", len(then)+1, maxPlanSteps)
	}
	var out []PlanThen
	for i, pt := range then {
		label := "step " + strconv.Itoa(i+2)
		if pt.Type == "" {
			return nil, fmt.Errorf("%s names no task type", label)
		}
		t, ok := allowed[domain.CapabilityName(pt.Type)]
		if !ok {
			return nil, fmt.Errorf("%s: %q isn't a task type the planner may use", label, pt.Type)
		}
		if t.Inputs.Max == 0 {
			return nil, fmt.Errorf("%s: %s takes no files, so it can't work on the results of the step before", label, t.Name)
		}
		params, err := stepParams(&proposedStep{Type: pt.Type, Params: pt.Params})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		mode := pt.Mode
		if mode == "" {
			mode = planOnce
		}
		if mode != planOnce && mode != planPerFile {
			return nil, fmt.Errorf("%s: unknown mode %q (once or perFile)", label, pt.Mode)
		}
		// A type that takes one file per run can only run once per file.
		if t.Inputs.Max == 1 {
			mode = planPerFile
		}
		out = append(out, PlanThen{PlanStep: PlanStep{Type: t.Name, Title: t.Title, Params: params}, Mode: mode})
	}
	return out, nil
}

// workflowSpec is the workflow a draft with follow-up steps describes:
// its first stage is the job jobSpec would build, then one stage per
// follow-up.
func (d planDraft) workflowSpec(files []domain.ArtifactRef) WorkflowSpec {
	byName := map[string]domain.ArtifactRef{}
	for _, f := range files {
		byName[f.Name] = f
	}
	var use []domain.ArtifactRef
	for _, name := range d.files {
		use = append(use, byName[name])
	}
	first := domain.WorkflowStage{WorkflowStep: domain.WorkflowStep{Type: d.task.Type, Params: copyParams(d.task.Params)}, Mode: d.mode, Parts: d.parts}
	if d.combine != nil {
		first.Combine = &domain.WorkflowStep{Type: d.combine.Type, Params: copyParams(d.combine.Params)}
	}
	spec := WorkflowSpec{Inputs: use, Stages: []domain.WorkflowStage{first}}
	for _, t := range d.then {
		spec.Stages = append(spec.Stages, domain.WorkflowStage{WorkflowStep: domain.WorkflowStep{Type: t.Type, Params: copyParams(t.Params)}, Mode: t.Mode})
	}
	spec.Name = "Planned: " + workflowTitle(spec.Stages)
	return spec
}

// workflowSpecFromPlan rebuilds a proposed plan's workflow (for approval).
func (p *Plan) workflowSpecFromPlan() WorkflowSpec {
	d := planDraft{task: p.Task, combine: p.Combine, mode: p.Mode, parts: p.Parts, files: p.UseFiles, then: p.Then}
	return d.workflowSpec(p.Files)
}

// approveWorkflowPlan submits a proposed plan with follow-up steps as a
// workflow; SubmitWorkflow checks every step again, policy included.
func (s *Server) approveWorkflowPlan(ctx context.Context, id, actor string, spec WorkflowSpec) (Plan, error) {
	wf, err := s.SubmitWorkflow(ctx, spec)
	view, _ := s.plans.update(id, func(p *Plan) {
		p.approving = false
		if err == nil {
			p.State, p.WorkflowID = PlanApproved, wf.ID
		}
	})
	if err != nil {
		return Plan{}, err
	}
	s.audit(domain.AuditSecurity, "plan.approved", "", actor, map[string]any{"planId": id, "workflowId": string(wf.ID), "steps": len(wf.Stages)})
	s.audit(domain.AuditSecurity, "workflow.submitted", "", actor, map[string]any{"workflowId": string(wf.ID), "steps": len(wf.Stages), "files": len(wf.Inputs), "maxAttempts": wf.MaxAttempts, "planId": id})
	log.Printf("plan.approved: %s -> workflow %s", id, wf.ID)
	s.publishPlan(view)
	return view, nil
}

// thenSchema is the follow-up steps' schema: at most maxPlanSteps-1, each
// a type that takes files, with its params and a mode.
func thenSchema(types []plannable) string {
	var takers []plannable
	for _, t := range types {
		if t.Inputs.Max > 0 {
			takers = append(takers, t)
		}
	}
	if len(takers) == 0 {
		return `{"type":"array","maxItems":0}`
	}
	return `{"type":"array","maxItems":` + strconv.Itoa(maxPlanSteps-1) + `,"items":` +
		stepSchema(takers, `{"type":"string","enum":["once","perFile"]}`) + `}`
}
