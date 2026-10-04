package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// AI planner (roadmap item 12): "AI proposes, the harness enforces". A
// request in plain words, with uploaded files, goes to a local model as
// an ordinary llm.chat (placed, queued and held like any other work),
// together with the catalog types it may use and a JSON schema for its
// answer. The answer is only a proposal. The manager validates it
// deterministically, with the same checks a submitted job gets (types,
// parameters, files, policy, placement), and shows the validated
// structure. Nothing runs until an operator approves it; then it is an
// ordinary job.

const (
	planTTL        = time.Hour       // an unapproved proposal expires
	planDeadline   = 5 * time.Minute // the model must have answered by then
	maxPlanning    = 2               // at once: each holds its model's only slot
	maxPlans       = 100             // kept in memory; the oldest settled go first
	maxPlanRequest = 2000            // bytes of request text
	maxPlanFiles   = 64
	maxPlanAnswer  = 16 << 10 // bytes of the model's answer kept
)

// PlanState is where a plan stands.
type PlanState string

const (
	PlanPlanning PlanState = "planning" // the model is answering
	PlanProposed PlanState = "proposed" // validated, waiting for approval
	PlanRefused  PlanState = "refused"  // the model said it can't, or its answer failed validation
	PlanFailed   PlanState = "failed"   // no answer (device lost, too slow)
	PlanApproved PlanState = "approved" // submitted as a job
	PlanRejected PlanState = "rejected"
	PlanExpired  PlanState = "expired"
)

// How a plan's task runs over its files.
const (
	planOnce    = "once"    // one run with every file
	planPerFile = "perFile" // one run per file, spread over the fleet
	planParts   = "parts"   // one run per part (catalog Type.Parts)
)

// PlanStep is one validated step: a catalog type and its parameters.
type PlanStep struct {
	Type  domain.CapabilityName `json:"type"`
	Title string                `json:"title"`
	// Params are the values the model set; Effective is what will run
	// (defaults filled in, canonical form).
	Params    map[string]string `json:"params"`
	Effective map[string]string `json:"effective,omitempty"`
}

// Plan is one request and what became of it.
type Plan struct {
	ID      string               `json:"id"`
	State   PlanState            `json:"state"`
	Request string               `json:"request"`
	Model   string               `json:"model"`
	Files   []domain.ArtifactRef `json:"files"`
	// WorkloadID is the planning chat.
	WorkloadID domain.WorkloadID `json:"workloadId,omitempty"`
	// Summary and ModelReason are the model's own words: shown as such,
	// never trusted.
	Summary     string `json:"summary,omitempty"`
	ModelReason string `json:"modelReason,omitempty"`
	// Problem is the harness's reason a plan was refused or failed.
	Problem string `json:"problem,omitempty"`
	// Answer is the model's raw answer.
	Answer   string    `json:"answer,omitempty"`
	Task     *PlanStep `json:"task,omitempty"`
	Mode     string    `json:"mode,omitempty"`
	Parts    int       `json:"parts,omitempty"`
	UseFiles []string  `json:"useFiles,omitempty"`
	Combine  *PlanStep `json:"combine,omitempty"`
	// Tasks is how many tasks the job will have (not counting combine).
	Tasks     int          `json:"tasks,omitempty"`
	JobID     domain.JobID `json:"jobId,omitempty"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
	ExpiresAt time.Time    `json:"expiresAt,omitzero"`

	cancel    context.CancelFunc // ends planning early (reject)
	approving bool               // an approval is submitting the job
}

// Planner errors the API maps to statuses.
var (
	ErrUnknownPlan     = errors.New("manager: unknown plan")
	ErrPlanState       = errors.New("manager: the plan can't do that now")
	ErrPlanBusy        = errors.New("manager: already planning as much as it can")
	ErrPlanUnavailable = errors.New("manager: no device can plan right now")
)

type planTable struct {
	mu    sync.Mutex
	plans map[string]*Plan
	order []string // oldest first
}

func newPlanTable() *planTable { return &planTable{plans: map[string]*Plan{}} }

// expireLocked settles proposals past their time.
func (t *planTable) expireLocked(now time.Time) {
	for _, p := range t.plans {
		if p.State == PlanProposed && !p.approving && now.After(p.ExpiresAt) {
			p.State, p.UpdatedAt = PlanExpired, now
		}
	}
}

// addLocked stores p, dropping the oldest settled plans beyond maxPlans.
func (t *planTable) addLocked(p *Plan) {
	t.plans[p.ID] = p
	t.order = append(t.order, p.ID)
	for i := 0; len(t.plans) > maxPlans && i < len(t.order); {
		old := t.plans[t.order[i]]
		if old.State == PlanPlanning || old.State == PlanProposed {
			i++
			continue
		}
		delete(t.plans, t.order[i])
		t.order = append(t.order[:i], t.order[i+1:]...)
	}
}

func (t *planTable) removeLocked(id string) {
	delete(t.plans, id)
	for i, o := range t.order {
		if o == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

func (t *planTable) get(id string) (Plan, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(time.Now())
	p, ok := t.plans[id]
	if !ok {
		return Plan{}, false
	}
	return *p, true
}

// list returns every plan, newest first.
func (t *planTable) list() []Plan {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(time.Now())
	out := make([]Plan, 0, len(t.order))
	for i := len(t.order) - 1; i >= 0; i-- {
		out = append(out, *t.plans[t.order[i]])
	}
	return out
}

// update applies f to plan id under the lock and returns the result.
func (t *planTable) update(id string, f func(p *Plan)) (Plan, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.plans[id]
	if !ok {
		return Plan{}, false
	}
	f(p)
	p.UpdatedAt = time.Now()
	return *p, true
}

// PlanRequest is a request to plan a job.
type PlanRequest struct {
	Request string `json:"request"`
	Model   string `json:"model"`
	Files   []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// planModels lists, per model, the READY devices that can plan with it:
// llm.chat with the model, from an agent that takes "format".
func (s *Server) planModels() map[string][]domain.NodeID {
	out := map[string][]domain.NodeID{}
	for m, ids := range s.chatModels() {
		for _, id := range ids {
			if rec, ok := s.Registry.Get(id); ok && capAttr(rec, capLLMChat, catalog.AttrChatFormat) != "" {
				out[m] = append(out[m], id)
			}
		}
	}
	return out
}

func capAttr(rec *NodeRecord, capability domain.CapabilityName, attr string) string {
	for _, c := range rec.Capabilities {
		if c.Name == capability {
			return c.Attributes[attr]
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// plannable is a type the planner may offer, with the values its choice
// parameters (a model) can take on the fleet right now.
type plannable struct {
	catalog.Type
	choices map[string][]string // param name -> values
}

// plannableTypes lists the catalog types a plan may use: offered by a
// READY device, enabled by policy, never internal, device-specific or the
// chat itself (and never raw commands, which aren't catalog types).
func (s *Server) plannableTypes() []plannable {
	var out []plannable
	nodes := s.Registry.List()
	for _, t := range catalog.Types() {
		if t.Internal || t.TargetRequired || t.Name == capLLMChat || t.Name == "llm.inventory" {
			continue
		}
		if s.checkPolicy(t.Name) != nil {
			continue
		}
		offered := false
		choices := map[string]map[string]bool{}
		for _, rec := range nodes {
			if rec.State != domain.NodeReady || !offersVersion(rec, t.Name) {
				continue
			}
			offered = true
			for _, p := range t.Params {
				if p.ChoicesAttr == "" {
					continue
				}
				if choices[p.Name] == nil {
					choices[p.Name] = map[string]bool{}
				}
				for _, v := range attrValues(rec, t.Name, p.ChoicesAttr) {
					if p.ChoicesAttr == catalog.AttrModels {
						v = catalog.NormalizeModel(v)
					}
					choices[p.Name][v] = true
				}
			}
		}
		if !offered {
			continue
		}
		pt := plannable{Type: t, choices: map[string][]string{}}
		usable := true
		for _, p := range t.Params {
			if p.ChoicesAttr == "" {
				continue
			}
			if len(choices[p.Name]) == 0 {
				usable = false // e.g. llm.generate with no model anywhere
				break
			}
			pt.choices[p.Name] = sortedKeys(choices[p.Name])
		}
		if usable {
			out = append(out, pt)
		}
	}
	return out
}

// StartPlan asks a model to plan req. It returns at once with the plan
// in state planning; the answer is validated when it arrives.
func (s *Server) StartPlan(ctx context.Context, req PlanRequest, actor string) (Plan, error) {
	text := strings.TrimSpace(req.Request)
	switch {
	case text == "":
		return Plan{}, fmt.Errorf("%w: say what you want done", ErrInvalidWorkload)
	case len(text) > maxPlanRequest:
		return Plan{}, fmt.Errorf("%w: the request is %d bytes; at most %d", ErrInvalidWorkload, len(text), maxPlanRequest)
	case !utf8.ValidString(text):
		return Plan{}, fmt.Errorf("%w: the request isn't valid text", ErrInvalidWorkload)
	case len(req.Files) > maxPlanFiles:
		return Plan{}, fmt.Errorf("%w: at most %d files", ErrInvalidWorkload, maxPlanFiles)
	}
	files := make([]domain.ArtifactRef, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, domain.ArtifactRef{Name: f.Name, SHA256: f.SHA256})
	}
	if len(files) > 0 {
		if s.cfg.Artifacts == nil {
			return Plan{}, fmt.Errorf("%w: this manager has no artifact store", ErrInvalidWorkload)
		}
		if err := domain.ValidateWorkloadFiles(files, nil); err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrInvalidWorkload, err)
		}
		for _, f := range files {
			if _, err := s.cfg.Artifacts.Stat(f.SHA256); err != nil {
				return Plan{}, fmt.Errorf("%w: file %s: %v", ErrInvalidWorkload, f.Name, err)
			}
		}
	}

	models := s.planModels()
	model := catalog.NormalizeModel(req.Model)
	holders, ok := models[model]
	if !ok {
		have := "no ready device offers a model the planner can use (it needs an updated agent and a local model)"
		if len(models) > 0 {
			have = "models that can plan: " + strings.Join(sortedKeys(models), ", ")
		}
		if model == "" {
			return Plan{}, fmt.Errorf("%w: pick a model (%s)", ErrInvalidWorkload, have)
		}
		return Plan{}, fmt.Errorf("%w: no ready device can plan with %q (%s)", ErrInvalidWorkload, req.Model, have)
	}
	if err := s.checkPolicy(capLLMChat); err != nil {
		return Plan{}, err
	}
	// Every device with the model holding work back: say so now rather
	// than leave the plan waiting in the queue.
	var held []string
	available := false
	for _, id := range holders {
		if rec, ok := s.Registry.Get(id); ok {
			if ok, reason := s.availableNow(rec, time.Now()); ok {
				available = true
				break
			} else {
				held = append(held, s.nodeDisplayName(id)+": "+reason)
			}
		}
	}
	if !available {
		return Plan{}, fmt.Errorf("%w: no device with %q is taking work right now (%s)", ErrPlanUnavailable, model, strings.Join(held, "; "))
	}
	types := s.plannableTypes()
	if len(types) == 0 {
		return Plan{}, fmt.Errorf("%w: no task type the planner may use is enabled and offered by a ready device", ErrInvalidWorkload)
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	msgs, _ := json.Marshal([]chatMessage{{"system", planPrompt(types, names)}, {"user", text}})
	params := map[string]string{
		"model": model, "messages": string(msgs), "format": planSchema(types, names),
		"temperature": "0.2", "max_tokens": "1024",
	}

	id, err := newRandomID()
	if err != nil {
		return Plan{}, err
	}
	now := time.Now()
	p := &Plan{ID: id[:16], State: PlanPlanning, Request: text, Model: model, Files: files, CreatedAt: now, UpdatedAt: now}
	// Reserve a planning place before submitting, so two requests can't
	// both slip under the limit.
	s.plans.mu.Lock()
	planning := 0
	for _, q := range s.plans.plans {
		if q.State == PlanPlanning {
			planning++
		}
	}
	if planning >= maxPlanning {
		s.plans.mu.Unlock()
		return Plan{}, fmt.Errorf("%w (%d plans at once); wait for one to finish", ErrPlanBusy, maxPlanning)
	}
	s.plans.addLocked(p)
	s.plans.mu.Unlock()

	// Subscribed before submitting, so no event about the chat slips by.
	events, unsubscribe := s.Events.Subscribe(64)
	wl, err := s.Submit(context.WithoutCancel(ctx), WorkloadSpec{Capability: capLLMChat, Params: params})
	if err != nil {
		unsubscribe()
		s.plans.mu.Lock()
		s.plans.removeLocked(p.ID)
		s.plans.mu.Unlock()
		return Plan{}, err
	}
	planCtx, cancel := context.WithTimeout(context.Background(), planDeadline)
	view, _ := s.plans.update(p.ID, func(p *Plan) { p.WorkloadID, p.cancel = wl.ID, cancel })
	// Never the request text: it can hold anything.
	s.audit(domain.AuditSecurity, "plan.requested", "", actor, map[string]any{"planId": p.ID, "model": model, "files": len(files), "workloadId": string(wl.ID)})
	log.Printf("plan.requested: %s (%s, %d files)", p.ID, model, len(files))
	s.publishPlan(view)
	go s.runPlan(planCtx, cancel, view, events, unsubscribe, types)
	return view, nil
}

func (s *Server) publishPlan(p Plan) {
	s.publish(domain.EventPlanUpdated, "", map[string]any{"planId": p.ID, "state": string(p.State)})
}

// runPlan waits for the model's answer and settles the plan.
func (s *Server) runPlan(ctx context.Context, cancel context.CancelFunc, p Plan, events <-chan domain.Event, unsubscribe func(), types []plannable) {
	defer unsubscribe()
	defer cancel()
	c := &chatRun{s: s, id: p.WorkloadID, events: events, model: p.Model}
	var answer strings.Builder
	res, err := c.follow(ctx, func(delta string) error {
		if answer.Len() < maxPlanAnswer {
			answer.WriteString(delta)
		}
		return nil
	})
	settle := func(f func(p *Plan)) {
		view, ok := s.plans.update(p.ID, func(p *Plan) {
			if p.State == PlanPlanning { // not rejected meanwhile
				f(p)
			}
			p.cancel = nil
		})
		if ok {
			log.Printf("plan.%s: %s %s", view.State, view.ID, view.Problem)
			s.publishPlan(view)
		}
	}
	if err != nil { // the deadline, or rejected while planning
		c.abandon(context.Background())
		settle(func(p *Plan) {
			p.State, p.Problem = PlanFailed, fmt.Sprintf("the model didn't answer within %s", planDeadline)
		})
		return
	}
	if res.err != "" {
		settle(func(p *Plan) { p.State, p.Problem = PlanFailed, "the model didn't answer: "+res.err })
		return
	}
	text := answer.String()
	draft, problem := draftPlan(text, p.Files, types)
	var spec JobSpec
	if problem == nil && draft.possible {
		spec = draft.jobSpec(p.Files)
		if err := s.checkJob(&spec); err != nil {
			problem = err
		}
	}
	settle(func(p *Plan) {
		p.Answer, p.Summary, p.ModelReason = text, draft.summary, draft.reason
		p.Task, p.Mode, p.Parts, p.UseFiles, p.Combine = draft.task, draft.mode, draft.parts, draft.files, draft.combine
		switch {
		case problem != nil:
			p.State, p.Problem = PlanRefused, strings.TrimPrefix(problem.Error(), ErrInvalidWorkload.Error()+": ")
		case !draft.possible:
			p.State, p.Problem = PlanRefused, "the model says the available task types can't do this"
		default:
			p.State, p.Tasks, p.ExpiresAt = PlanProposed, len(spec.Tasks), time.Now().Add(planTTL)
			// What will run: the checked job's compiled parameters.
			p.Task.Effective = spec.Tasks[0].Params
			if p.Mode == planParts {
				delete(p.Task.Effective, "part")
			}
			if p.Combine != nil && spec.Reduce != nil {
				p.Combine.Effective = spec.Reduce.Params
			}
		}
	})
}

// ApprovePlan submits a proposed plan as a job.
func (s *Server) ApprovePlan(ctx context.Context, id, actor string) (Plan, error) {
	s.plans.mu.Lock()
	s.plans.expireLocked(time.Now())
	p, ok := s.plans.plans[id]
	if !ok {
		s.plans.mu.Unlock()
		return Plan{}, ErrUnknownPlan
	}
	if p.State != PlanProposed || p.approving {
		state := p.State
		s.plans.mu.Unlock()
		return Plan{}, fmt.Errorf("%w: the plan is %s", ErrPlanState, state)
	}
	p.approving = true
	spec := p.jobSpecFromPlan()
	s.plans.mu.Unlock()

	// SubmitJob checks everything again: policy may have changed since.
	job, err := s.SubmitJob(ctx, spec)
	view, _ := s.plans.update(id, func(p *Plan) {
		p.approving = false
		if err == nil {
			p.State, p.JobID = PlanApproved, job.ID
		}
	})
	if err != nil {
		return Plan{}, err
	}
	s.audit(domain.AuditSecurity, "plan.approved", "", actor, map[string]any{"planId": id, "jobId": string(job.ID), "tasks": len(job.Tasks)})
	s.audit(domain.AuditSecurity, "job.submitted", "", actor, map[string]any{"jobId": string(job.ID), "tasks": len(job.Tasks), "reduce": job.Reduce != nil, "maxAttempts": job.MaxAttempts, "planId": id})
	log.Printf("plan.approved: %s -> job %s", id, job.ID)
	s.publishPlan(view)
	return view, nil
}

// RejectPlan discards a plan, stopping the model if it is still planning.
func (s *Server) RejectPlan(id, actor string) (Plan, error) {
	var cancel context.CancelFunc
	var stateErr error
	view, ok := s.plans.update(id, func(p *Plan) {
		switch {
		case p.approving:
			stateErr = fmt.Errorf("%w: the plan is being approved", ErrPlanState)
		case p.State == PlanPlanning || p.State == PlanProposed:
			p.State, cancel = PlanRejected, p.cancel
		default:
			stateErr = fmt.Errorf("%w: the plan is %s", ErrPlanState, p.State)
		}
	})
	if !ok {
		return Plan{}, ErrUnknownPlan
	}
	if stateErr != nil {
		return Plan{}, stateErr
	}
	if cancel != nil {
		cancel() // runPlan cancels the chat
	}
	s.audit(domain.AuditSecurity, "plan.rejected", "", actor, map[string]any{"planId": id})
	s.publishPlan(view)
	return view, nil
}

// ---- the model's answer

// proposal is the answer's shape (planSchema).
type proposal struct {
	Summary  string          `json:"summary"`
	Task     *proposedStep   `json:"task"`
	Files    []string        `json:"files"`
	Mode     string          `json:"mode"`
	Parts    json.RawMessage `json:"parts"`
	Combine  *proposedStep   `json:"combine"`
	Possible *bool           `json:"possible"`
	Reason   string          `json:"reason"`
}

type proposedStep struct {
	Type   string                     `json:"type"`
	Params map[string]json.RawMessage `json:"params"`
}

// planDraft is a structurally valid proposal, before the job checks.
type planDraft struct {
	summary, reason string
	possible        bool
	task, combine   *PlanStep
	mode            string
	parts           int
	files           []string
}

// extractJSON finds the JSON object in a model's answer, which may come
// wrapped in prose or a code fence.
func extractJSON(answer string) (string, bool) {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start {
		return "", false
	}
	return answer[start : end+1], true
}

// scalar reads one JSON scalar as a parameter string ("" for null).
func scalar(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	}
	return "", errors.New("must be a single value, not a list or an object")
}

func stepParams(step *proposedStep) (map[string]string, error) {
	out := make(map[string]string, len(step.Params))
	for name, raw := range step.Params {
		v, err := scalar(raw)
		if err != nil {
			return nil, fmt.Errorf("%s parameter %q %v", step.Type, name, err)
		}
		if v != "" {
			out[name] = v
		}
	}
	return out, nil
}

// draftPlan parses and checks a model's answer against the types it was
// offered and the request's files. A non-nil error is why it's refused.
func draftPlan(answer string, files []domain.ArtifactRef, types []plannable) (planDraft, error) {
	var d planDraft
	raw, ok := extractJSON(answer)
	if !ok {
		return d, errors.New("the model's answer isn't JSON")
	}
	var prop proposal
	if err := json.Unmarshal([]byte(raw), &prop); err != nil {
		return d, fmt.Errorf("the model's answer isn't the JSON asked for: %v", err)
	}
	d.summary, d.reason = strings.TrimSpace(prop.Summary), strings.TrimSpace(prop.Reason)
	d.possible = prop.Possible == nil || *prop.Possible
	if !d.possible {
		return d, nil
	}
	allowed := make(map[domain.CapabilityName]catalog.Type, len(types))
	for _, t := range types {
		allowed[t.Name] = t.Type
	}
	if prop.Task == nil || prop.Task.Type == "" {
		return d, errors.New("the model's answer names no task")
	}
	t, ok := allowed[domain.CapabilityName(prop.Task.Type)]
	if !ok {
		return d, fmt.Errorf("%q isn't a task type the planner may use", prop.Task.Type)
	}
	params, err := stepParams(prop.Task)
	if err != nil {
		return d, err
	}
	d.task = &PlanStep{Type: t.Name, Title: t.Title, Params: params}

	given := map[string]bool{}
	for _, f := range files {
		given[f.Name] = true
	}
	used := map[string]bool{}
	for _, name := range prop.Files {
		if !given[name] {
			return d, fmt.Errorf("the plan uses %q, which isn't one of the files given", name)
		}
		if used[name] {
			return d, fmt.Errorf("the plan names %q twice", name)
		}
		used[name] = true
		d.files = append(d.files, name)
	}
	// Files given with the request but none named: a task that takes
	// files gets them all (the proposal shows exactly which).
	if len(d.files) == 0 && t.Inputs.Max > 0 {
		for _, f := range files {
			d.files = append(d.files, f.Name)
		}
	}

	d.mode = prop.Mode
	if d.mode == "" {
		d.mode = planOnce
	}
	// A type that takes one file per run, given several: once per file
	// is the only way to run it.
	if d.mode == planOnce && t.Inputs.Max == 1 && len(d.files) > 1 {
		d.mode = planPerFile
	}
	if len(prop.Parts) > 0 {
		v, err := scalar(prop.Parts)
		if err != nil {
			return d, fmt.Errorf("parts %v", err)
		}
		if v != "" {
			if d.parts, err = strconv.Atoi(v); err != nil {
				return d, fmt.Errorf("parts must be a whole number, not %q", v)
			}
		}
	}
	if t.Parts {
		// The part counters are the plan's to set: a strip-rendering
		// task asked for in N parts runs N times, never as one strip.
		if n, err := strconv.Atoi(params["parts"]); err == nil && n > 1 && (d.mode != planParts || d.parts < 1) {
			d.mode, d.parts = planParts, n
		}
		if d.parts > 1 {
			d.mode = planParts
		}
		delete(params, "part")
		delete(params, "parts")
	}
	// Once per file, for a type that takes no files, can only mean once.
	if d.mode == planPerFile && t.Inputs.Max == 0 {
		d.mode = planOnce
	}
	switch d.mode {
	case planOnce:
		d.parts = 0
	case planPerFile:
		d.parts = 0
		if len(d.files) == 0 {
			return d, errors.New("the plan runs once per file, but no files were given")
		}
	case planParts:
		if !t.Parts {
			return d, fmt.Errorf("%s can't be split into parts", t.Name)
		}
		if d.parts < 1 || d.parts > domain.MaxJobTasks {
			return d, fmt.Errorf("parts must be 1-%d, not %d", domain.MaxJobTasks, d.parts)
		}
	default:
		return d, fmt.Errorf("unknown mode %q (once, perFile or parts)", prop.Mode)
	}

	if prop.Combine != nil && prop.Combine.Type != "" {
		ct, ok := allowed[domain.CapabilityName(prop.Combine.Type)]
		if !ok || !ct.Reduce {
			return d, fmt.Errorf("%q isn't a type the planner may combine results with", prop.Combine.Type)
		}
		cp, err := stepParams(prop.Combine)
		if err != nil {
			return d, err
		}
		d.combine = &PlanStep{Type: ct.Name, Title: ct.Title, Params: cp}
	}
	return d, nil
}

func copyParams(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// jobSpec is the job the draft describes.
func (d planDraft) jobSpec(files []domain.ArtifactRef) JobSpec {
	byName := map[string]domain.ArtifactRef{}
	for _, f := range files {
		byName[f.Name] = f
	}
	var use []domain.ArtifactRef
	for _, name := range d.files {
		use = append(use, byName[name])
	}
	spec := JobSpec{Name: "Planned: " + d.task.Title}
	switch d.mode {
	case planPerFile:
		for _, f := range use {
			spec.Tasks = append(spec.Tasks, domain.TaskSpec{Name: f.Name, Capability: d.task.Type, Params: copyParams(d.task.Params), Inputs: []domain.ArtifactRef{f}})
		}
		spec.Name += fmt.Sprintf(", once per file (%d)", len(use))
	case planParts:
		for i := 0; i < d.parts; i++ {
			params := copyParams(d.task.Params)
			params["part"], params["parts"] = strconv.Itoa(i), strconv.Itoa(d.parts)
			spec.Tasks = append(spec.Tasks, domain.TaskSpec{Name: fmt.Sprintf("part %d", i+1), Capability: d.task.Type, Params: params, Inputs: append([]domain.ArtifactRef(nil), use...)})
		}
		spec.Name += fmt.Sprintf(", in %d parts", d.parts)
	default:
		spec.Tasks = []domain.TaskSpec{{Name: d.task.Title, Capability: d.task.Type, Params: copyParams(d.task.Params), Inputs: use}}
	}
	if d.combine != nil {
		spec.Reduce = &domain.TaskSpec{Name: d.combine.Title, Capability: d.combine.Type, Params: copyParams(d.combine.Params)}
		spec.Name += ", then " + strings.ToLower(d.combine.Title[:1]) + d.combine.Title[1:]
	}
	return spec
}

// jobSpecFromPlan rebuilds a proposed plan's job (for approval).
func (p *Plan) jobSpecFromPlan() JobSpec {
	d := planDraft{task: p.Task, combine: p.Combine, mode: p.Mode, parts: p.Parts, files: p.UseFiles}
	return d.jobSpec(p.Files)
}

// ---- the prompt

// describeParam is one parameter as the model reads it.
func describeParam(p catalog.Param, choices []string) string {
	var kind string
	rng := func() string {
		switch {
		case p.Min != nil && p.Max != nil:
			return fmt.Sprintf(" %g-%g", *p.Min, *p.Max)
		case p.Min != nil:
			return fmt.Sprintf(" from %g", *p.Min)
		case p.Max != nil:
			return fmt.Sprintf(" up to %g", *p.Max)
		}
		return ""
	}
	switch p.Type {
	case catalog.Int:
		kind = "whole number" + rng()
	case catalog.Number:
		kind = "number" + rng()
	case catalog.Bool:
		kind = "true or false"
	case catalog.Enum:
		kind = "one of " + strings.Join(p.Enum, ", ")
	default:
		switch {
		case len(choices) > 0:
			kind = "one of " + strings.Join(choices, ", ")
		case p.Pattern != "":
			kind = "text matching " + p.Pattern
		default:
			kind = "text"
		}
	}
	s := p.Name
	if p.Title != "" {
		s += " (" + p.Title + ")"
	}
	s += ": " + kind
	if p.Default != "" {
		s += ", default " + p.Default
	} else if p.Required {
		s += ", required"
	}
	return s
}

// planPrompt is the planning chat's system message.
func planPrompt(types []plannable, files []string) string {
	var b strings.Builder
	b.WriteString("You turn a request into a job for a home computer cluster. You may only use these task types:\n\n")
	for _, t := range types {
		marks := ""
		if t.Reduce {
			marks += " [can combine]"
		}
		if t.Parts {
			marks += " [parts]"
		}
		fmt.Fprintf(&b, "%s%s: %s. %s\n", t.Name, marks, t.Title, t.Description)
		var ps []string
		for _, p := range plannedParams(t) {
			ps = append(ps, describeParam(p, t.choices[p.Name]))
		}
		if len(ps) > 0 {
			b.WriteString("  params: " + strings.Join(ps, "; ") + "\n")
		}
		switch in := t.Inputs; {
		case in.Max == 0:
			b.WriteString("  files: none\n")
		case in.Max == 1:
			b.WriteString("  files: exactly 1 per run")
		default:
			fmt.Fprintf(&b, "  files: %d-%d per run", in.Min, in.Max)
		}
		if t.Inputs.Max > 0 {
			if len(t.Inputs.Extensions) > 0 {
				b.WriteString(" (" + strings.Join(t.Inputs.Extensions, ", ") + ")")
			}
			b.WriteString("\n")
		}
	}
	if len(files) == 0 {
		b.WriteString("\nThe user gave no files.\n")
	} else {
		b.WriteString("\nThe user's files: " + strings.Join(files, ", ") + "\n")
	}
	b.WriteString(`
Answer with one JSON object:
- summary: one short sentence saying what the job does.
- task: {"type": one task type above, "params": {...}}. Set only the params the request asks for; the others keep their defaults.
- files: which of the user's files the task uses ([] if none). Files go here, never in params.
- mode: "perFile" runs the task once for each file, spread over the devices (needed when a task takes 1 file per run and there are several); "once" runs it once with all the files; "parts" (only types marked [parts]) splits the work into "parts" pieces done in parallel.
- parts: how many pieces for mode "parts", otherwise 0.
- combine: null, or {"type": a type marked [can combine], "params": {...}} to join every result into one file (for example one zip to download).
- possible: false if these task types can't do what was asked.
- reason: if possible is false, why; otherwise "".
`)
	return b.String()
}

// plannedParams are the parameters a plan sets: all but a Parts type's
// part counters, which the plan's mode sets.
func plannedParams(t plannable) []catalog.Param {
	var out []catalog.Param
	for _, p := range t.Params {
		if t.Parts && (p.Name == "part" || p.Name == "parts") {
			continue
		}
		out = append(out, p)
	}
	return out
}

func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

// paramSchema is one parameter as a JSON schema property: its type, range
// and choices, so the model can't write a value of the wrong kind.
func paramSchema(p catalog.Param, choices []string) string {
	bound := func(kind string) string {
		s := `{"type":"` + kind + `"`
		if p.Min != nil {
			s += `,"minimum":` + strconv.FormatFloat(*p.Min, 'f', -1, 64)
		}
		if p.Max != nil {
			s += `,"maximum":` + strconv.FormatFloat(*p.Max, 'f', -1, 64)
		}
		return s + "}"
	}
	switch p.Type {
	case catalog.Int:
		return bound("integer")
	case catalog.Number:
		return `{"type":"number"}`
	case catalog.Bool:
		return `{"type":"boolean"}`
	case catalog.Enum:
		return `{"type":"string","enum":` + jsonText(p.Enum) + `}`
	}
	if len(choices) > 0 {
		return `{"type":"string","enum":` + jsonText(choices) + `}`
	}
	return `{"type":"string"}`
}

// stepSchema is a plan step over the given types: one of their names,
// and params limited to theirs, each typed. (One object for every type,
// not an anyOf per type: with anyOf, gemma3:1b under Ollama's grammar
// stopped picking the type its own summary described.) Which parameter
// belongs to which type is the catalog's check, after the answer.
func stepSchema(types []plannable) string {
	var names, props []string
	seen := map[string]bool{}
	for _, t := range types {
		names = append(names, string(t.Name))
		for _, p := range plannedParams(t) {
			if seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			schema := paramSchema(p, t.choices[p.Name])
			// The same name in another type with another kind or range:
			// any value of a basic kind, the catalog checks the rest.
			for _, o := range types {
				for _, q := range plannedParams(o) {
					if q.Name == p.Name && paramSchema(q, o.choices[q.Name]) != schema {
						schema = `{"type":["string","number","boolean"]}`
					}
				}
			}
			props = append(props, jsonText(p.Name)+":"+schema)
		}
	}
	return `{"type":"object","properties":{"type":{"type":"string","enum":` + jsonText(names) + `},` +
		`"params":{"type":"object","properties":{` + strings.Join(props, ",") + `},"additionalProperties":false}},"required":["type","params"]}`
}

// planSchema is the JSON schema the answer must follow, its keys in the
// order the model should write them (built by hand: a Go map would sort
// them).
func planSchema(types []plannable, files []string) string {
	var combiners []plannable
	for _, t := range types {
		if t.Reduce {
			combiners = append(combiners, t)
		}
	}
	task := stepSchema(types)
	fileList := `{"type":"array","maxItems":0}`
	if len(files) > 0 {
		fileList = `{"type":"array","items":{"type":"string","enum":` + jsonText(files) + `}}`
	}
	combine := `{"type":"null"}`
	if len(combiners) > 0 {
		combine = `{"anyOf":[{"type":"null"},` + stepSchema(combiners) + `]}`
	}
	return `{"type":"object","properties":{` +
		`"summary":{"type":"string"},` +
		`"task":` + task + `,` +
		`"files":` + fileList + `,` +
		`"mode":{"type":"string","enum":["once","perFile","parts"]},` +
		`"parts":{"type":"integer","minimum":0,"maximum":` + strconv.Itoa(domain.MaxJobTasks) + `},` +
		`"combine":` + combine + `,` +
		`"possible":{"type":"boolean"},` +
		`"reason":{"type":"string"}` +
		`},"required":["summary","task","files","mode","parts","combine","possible","reason"]}`
}
