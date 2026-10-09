package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"home-harness/internal/cronspec"
	"home-harness/internal/domain"
)

// Scheduled and recurring work: a job or a workflow submitted on a cron
// expression, an @macro or "every 15m".
//
// Exactly one run per slot, across crashes and restarts. A slot is an
// instant the expression names; the job or workflow a slot starts is
// tagged with it (Job.Schedule/Slot, Workflow.Schedule/Slot) and the job
// or workflow record is persisted by SubmitJob/SubmitWorkflow before the
// schedule's own record is. A crash between the two leaves the work
// started and the cursor behind; the next pass finds the tag, links the
// run into the history and moves the cursor on instead of submitting
// again. The cursor (LastSlot), run count, the slot queued by overlap
// "queue", and the history live in one record, written in one transaction.
//
// Time. Slots are found from wall-clock time only (the monotonic reading
// is stripped), and no slot at or before LastSlot ever fires, so a clock
// that jumps backwards cannot repeat a run (it waits until the clock
// passes the next slot), and one that jumps forwards runs the latest slot
// once and handles the others under the missed policy. A slot is on time
// within scheduleGrace of its instant; a later one counts as missed.
//
// Runs go through SubmitJob/SubmitWorkflow like any other submission:
// policy, typed parameter checks, files present and a device able to run
// it are checked at run time, and a refusal (a type disabled since, a
// file gone, no device ready) is recorded as a failed run with the
// reason. Nothing is retried or bypassed: the slot is used up.

const (
	// scheduleGrace is how late a slot may be picked up and still count as
	// on time (a busy pass, a manager that took a moment to start).
	scheduleGrace = 90 * time.Second
	// maxDueSlots bounds the slots counted in one pass after a long
	// downtime; the cursor then jumps to the latest.
	maxDueSlots = 5000
)

// scheduleStore is what a store needs for schedules to survive a restart
// (internal/store/persistent has it); without one they live in memory.
type scheduleStore interface {
	UpsertSchedule(sc domain.Schedule) error
	DeleteSchedule(id domain.ScheduleID) error
	ListSchedules() ([]domain.Schedule, error)
}

type scheduleTable struct {
	load sync.Once
	mu   sync.Mutex
	byID map[domain.ScheduleID]*domain.Schedule
	// runMu serializes everything that reads and then changes a schedule
	// (a pass, a manual run, pause, resume), so one slot is never
	// submitted twice by two goroutines.
	runMu sync.Mutex
	now   func() time.Time
}

func newScheduleTable() *scheduleTable {
	return &scheduleTable{byID: map[domain.ScheduleID]*domain.Schedule{}, now: time.Now}
}

// ErrUnknownSchedule is a schedule ID the manager doesn't know.
var ErrUnknownSchedule = errors.New("manager: unknown schedule")

// ErrScheduleState is an action that doesn't fit the schedule's state.
var ErrScheduleState = errors.New("manager: schedule is in the wrong state for that")

// scheduleTbl returns the table, loading it from the store the first
// time. Nil on a bare Server (unit tests that don't use schedules).
func (s *Server) scheduleTbl() *scheduleTable {
	t := s.schedules
	if t == nil {
		return nil
	}
	t.load.Do(func() {
		st, ok := s.store.(scheduleStore)
		if !ok {
			return
		}
		list, err := st.ListSchedules()
		if err != nil {
			log.Printf("manager: load schedules: %v", err)
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		for i := range list {
			sc := list[i]
			t.byID[sc.ID] = &sc
		}
	})
	return t
}

func (t *scheduleTable) get(id domain.ScheduleID) (domain.Schedule, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sc, ok := t.byID[id]
	if !ok {
		return domain.Schedule{}, false
	}
	return *sc, true
}

func (t *scheduleTable) put(sc domain.Schedule) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := sc
	t.byID[sc.ID] = &c
}

func (t *scheduleTable) list() []domain.Schedule {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]domain.Schedule, 0, len(t.byID))
	for _, sc := range t.byID {
		out = append(out, *sc)
	}
	sort.Slice(out, func(i, k int) bool {
		if !out[i].CreatedAt.Equal(out[k].CreatedAt) {
			return out[i].CreatedAt.Before(out[k].CreatedAt)
		}
		return out[i].ID < out[k].ID
	})
	return out
}

// save records sc in memory and in the store (one write).
func (s *Server) saveSchedule(sc domain.Schedule) {
	t := s.scheduleTbl()
	t.put(sc)
	if st, ok := s.store.(scheduleStore); ok {
		if err := st.UpsertSchedule(sc); err != nil {
			log.Printf("manager: persist schedule %s: %v", sc.ID, err)
		}
	}
}

func slotKey(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// scheduleSpec parses a schedule's expression in its zone.
func scheduleSpec(sc domain.Schedule) (*cronspec.Spec, error) {
	loc, err := cronspec.LoadLocation(sc.Timezone)
	if err != nil {
		return nil, err
	}
	return cronspec.Parse(sc.Spec, sc.Anchor, loc)
}

// ScheduleRequest is a new schedule.
type ScheduleRequest struct {
	Name     string
	Spec     string
	Timezone string
	Missed   string
	Overlap  string
	MaxRuns  int
	EndAt    time.Time
	Job      *domain.JobTemplate
	Workflow *domain.WorkflowTemplate
}

func cloneJSON[T any](v T) T {
	raw, _ := json.Marshal(v)
	var out T
	json.Unmarshal(raw, &out)
	return out
}

// checkScheduleTemplate checks the work a schedule would submit exactly
// as a submission is checked now (on copies; nothing is recorded).
func (s *Server) checkScheduleTemplate(sc domain.Schedule) error {
	if sc.Workflow != nil {
		spec := WorkflowSpec{Name: sc.Workflow.Name, Inputs: cloneJSON(sc.Workflow.Inputs), Stages: cloneJSON(sc.Workflow.Stages), MaxAttempts: sc.Workflow.MaxAttempts}
		_, err := s.checkWorkflow(&spec)
		return err
	}
	spec := jobSpecFromTemplate(sc, "")
	return s.checkJob(&spec)
}

func jobSpecFromTemplate(sc domain.Schedule, slot string) JobSpec {
	t := cloneJSON(*sc.Job)
	spec := JobSpec{Name: t.Name, Tasks: t.Tasks, Reduce: t.Reduce, MaxAttempts: t.MaxAttempts, Priority: t.Priority, Schedule: string(sc.ID), Slot: slot}
	if spec.Name == "" {
		spec.Name = sc.Name
	}
	return spec
}

// CreateSchedule validates and stores a new schedule. The expression,
// zone, policies and the work itself are all checked now.
func (s *Server) CreateSchedule(ctx context.Context, req ScheduleRequest) (domain.Schedule, error) {
	t := s.scheduleTbl()
	if t == nil {
		return domain.Schedule{}, errors.New("manager: schedules are not available")
	}
	bad := func(format string, a ...any) (domain.Schedule, error) {
		return domain.Schedule{}, fmt.Errorf("%w: %s", ErrInvalidWorkload, fmt.Sprintf(format, a...))
	}
	if len(req.Name) > 80 || strings.ContainsAny(req.Name, "\x00\r\n") {
		return bad("the name is up to 80 characters on one line")
	}
	if (req.Job == nil) == (req.Workflow == nil) {
		return bad("give either a job or a workflow to run")
	}
	missed, overlap := req.Missed, req.Overlap
	if missed == "" {
		missed = domain.MissedSkip
	}
	if overlap == "" {
		overlap = domain.OverlapSkip
	}
	if missed != domain.MissedSkip && missed != domain.MissedRunOnce {
		return bad("missed is %q or %q", domain.MissedSkip, domain.MissedRunOnce)
	}
	if overlap != domain.OverlapSkip && overlap != domain.OverlapQueue && overlap != domain.OverlapAllow {
		return bad("overlap is %q, %q or %q", domain.OverlapSkip, domain.OverlapQueue, domain.OverlapAllow)
	}
	if req.MaxRuns < 0 || req.MaxRuns > 1_000_000 {
		return bad("maxRuns is 1-1000000, or 0 for no limit")
	}
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	now := t.now().UTC().Round(0)
	if !req.EndAt.IsZero() && !req.EndAt.After(now) {
		return bad("the end time is already past")
	}
	id, err := newRandomID()
	if err != nil {
		return domain.Schedule{}, err
	}
	sc := domain.Schedule{
		ID: domain.ScheduleID(id), Name: req.Name, Spec: strings.TrimSpace(req.Spec), Timezone: tz, Anchor: now,
		Job: req.Job, Workflow: req.Workflow, Missed: missed, Overlap: overlap, MaxRuns: req.MaxRuns, EndAt: req.EndAt.UTC(),
		State: domain.ScheduleActive, CreatedAt: now, LastSlot: now,
	}
	if _, err := scheduleSpec(sc); err != nil {
		return bad("%v", err)
	}
	if err := s.checkScheduleTemplate(sc); err != nil {
		return domain.Schedule{}, err
	}
	t.runMu.Lock()
	defer t.runMu.Unlock()
	if len(t.list()) >= domain.MaxSchedules {
		return bad("at most %d schedules", domain.MaxSchedules)
	}
	s.saveSchedule(sc)
	log.Printf("schedule.created: %s %q %s (%s)", sc.ID, sc.Name, sc.Spec, sc.Timezone)
	return sc, nil
}

// PauseSchedule stops a schedule firing; ResumeSchedule starts it again
// from now (the paused time is not caught up).
func (s *Server) PauseSchedule(id domain.ScheduleID) (domain.Schedule, error) {
	return s.changeSchedule(id, func(sc *domain.Schedule, now time.Time) error {
		if sc.State != domain.ScheduleActive {
			return fmt.Errorf("%w: it is %s", ErrScheduleState, sc.State)
		}
		sc.State = domain.SchedulePaused
		return nil
	})
}

func (s *Server) ResumeSchedule(id domain.ScheduleID) (domain.Schedule, error) {
	return s.changeSchedule(id, func(sc *domain.Schedule, now time.Time) error {
		if sc.State != domain.SchedulePaused {
			return fmt.Errorf("%w: it is %s", ErrScheduleState, sc.State)
		}
		sc.State = domain.ScheduleActive
		if now.After(sc.LastSlot) {
			sc.LastSlot = now
		}
		return nil
	})
}

func (s *Server) changeSchedule(id domain.ScheduleID, f func(*domain.Schedule, time.Time) error) (domain.Schedule, error) {
	t := s.scheduleTbl()
	if t == nil {
		return domain.Schedule{}, ErrUnknownSchedule
	}
	t.runMu.Lock()
	defer t.runMu.Unlock()
	sc, ok := t.get(id)
	if !ok {
		return domain.Schedule{}, ErrUnknownSchedule
	}
	if err := f(&sc, t.now().UTC().Round(0)); err != nil {
		return domain.Schedule{}, err
	}
	s.saveSchedule(sc)
	return sc, nil
}

// DeleteSchedule removes a schedule; runs already started carry on.
func (s *Server) DeleteSchedule(id domain.ScheduleID) error {
	t := s.scheduleTbl()
	if t == nil {
		return ErrUnknownSchedule
	}
	t.runMu.Lock()
	defer t.runMu.Unlock()
	if _, ok := t.get(id); !ok {
		return ErrUnknownSchedule
	}
	if st, ok := s.store.(scheduleStore); ok {
		if err := st.DeleteSchedule(id); err != nil {
			return fmt.Errorf("manager: delete schedule: %w", err)
		}
	}
	t.mu.Lock()
	delete(t.byID, id)
	t.mu.Unlock()
	return nil
}

// RunScheduleNow starts the schedule's work once, now, whatever its state
// and overlap policy. It doesn't move the cursor or count toward max runs.
func (s *Server) RunScheduleNow(ctx context.Context, id domain.ScheduleID) (domain.ScheduleRun, error) {
	t := s.scheduleTbl()
	if t == nil {
		return domain.ScheduleRun{}, ErrUnknownSchedule
	}
	t.runMu.Lock()
	defer t.runMu.Unlock()
	sc, ok := t.get(id)
	if !ok {
		return domain.ScheduleRun{}, ErrUnknownSchedule
	}
	now := t.now().UTC().Round(0)
	run := s.fireSchedule(ctx, sc, "manual-"+now.Format("20060102T150405.000000000Z"), now, true)
	sc.History = appendRun(sc.History, run)
	s.saveSchedule(sc)
	return run, nil
}

func appendRun(h []domain.ScheduleRun, r domain.ScheduleRun) []domain.ScheduleRun {
	h = append(h, r)
	if len(h) > domain.MaxScheduleHistory {
		h = append([]domain.ScheduleRun(nil), h[len(h)-domain.MaxScheduleHistory:]...)
	}
	return h
}

// fireSchedule submits one run of sc tagged with slot (a key from
// slotKey, or "manual-...") and returns its history entry.
func (s *Server) fireSchedule(ctx context.Context, sc domain.Schedule, slot string, at time.Time, manual bool) domain.ScheduleRun {
	run := domain.ScheduleRun{Slot: slot, At: at, Manual: manual, Outcome: domain.RunStarted}
	var err error
	if sc.Workflow != nil {
		tmpl := cloneJSON(*sc.Workflow)
		name := tmpl.Name
		if name == "" {
			name = sc.Name
		}
		var wf domain.Workflow
		wf, err = s.SubmitWorkflow(ctx, WorkflowSpec{Name: name, Inputs: tmpl.Inputs, Stages: tmpl.Stages, MaxAttempts: tmpl.MaxAttempts, Schedule: string(sc.ID), Slot: slot})
		run.Workflow = wf.ID
	} else {
		var job domain.Job
		job, err = s.SubmitJob(ctx, jobSpecFromTemplate(sc, slot))
		run.Job = job.ID
	}
	if err != nil {
		run.Outcome, run.Reason = domain.RunFailed, err.Error()
		run.Job, run.Workflow = "", ""
	}
	kind := "schedule.run"
	actor := "scheduler"
	log.Printf("schedule.run: %s slot %s %s %s", sc.ID, slot, run.Outcome, run.Reason)
	logName := domain.AuditNoise
	if manual {
		logName, actor = domain.AuditSecurity, actorFrom(ctx)
	}
	s.audit(logName, kind, "", actor, map[string]any{
		"scheduleId": string(sc.ID), "slot": slot, "outcome": run.Outcome, "reason": run.Reason,
		"jobId": string(run.Job), "workflowId": string(run.Workflow), "manual": manual,
	})
	return run
}

// scheduledRunOf finds the job or workflow already started for a slot.
func (s *Server) scheduledRunOf(id domain.ScheduleID, slot string) (domain.JobID, domain.WorkflowID, bool) {
	for _, j := range s.jobs.list() {
		if j.Schedule == string(id) && j.Slot == slot {
			return j.ID, "", true
		}
	}
	for _, wf := range s.workflows.list() {
		if wf.Schedule == string(id) && wf.Slot == slot {
			return "", wf.ID, true
		}
	}
	return "", "", false
}

// scheduleRunning: a job or workflow this schedule started is still going.
func (s *Server) scheduleRunning(id domain.ScheduleID) bool {
	for _, j := range s.jobs.list() {
		if j.Schedule == string(id) && j.State == domain.JobRunning {
			return true
		}
	}
	for _, wf := range s.workflows.list() {
		if wf.Schedule == string(id) && wf.State == domain.JobRunning {
			return true
		}
	}
	return false
}

// advanceSchedules is one pass over every active schedule, run by the
// reconcile loop.
func (s *Server) advanceSchedules(ctx context.Context) {
	t := s.scheduleTbl()
	if t == nil {
		return
	}
	for _, sc := range t.list() {
		if sc.State == domain.ScheduleActive {
			s.advanceSchedule(ctx, sc.ID)
		}
	}
}

func (s *Server) advanceSchedule(ctx context.Context, id domain.ScheduleID) {
	t := s.scheduleTbl()
	t.runMu.Lock()
	defer t.runMu.Unlock()
	sc, ok := t.get(id)
	if !ok || sc.State != domain.ScheduleActive {
		return
	}
	spec, err := scheduleSpec(sc)
	if err != nil { // can't happen: checked at creation
		log.Printf("manager: schedule %s: %v", sc.ID, err)
		return
	}
	now := t.now().UTC().Round(0)
	changed := false
	end := func(reason string) {
		sc.State = domain.ScheduleEnded
		log.Printf("schedule.ended: %s (%s)", sc.ID, reason)
		changed = true
	}
	defer func() {
		if changed {
			s.saveSchedule(sc)
		}
	}()
	record := func(r domain.ScheduleRun) {
		sc.History = appendRun(sc.History, r)
		changed = true
	}
	limitReached := func() bool { return sc.MaxRuns > 0 && sc.Runs >= sc.MaxRuns }

	// A slot waiting under overlap "queue" goes first, once the run before
	// it has finished.
	if !sc.QueuedSlot.IsZero() && !s.scheduleRunning(sc.ID) {
		slot := sc.QueuedSlot
		sc.QueuedSlot = time.Time{}
		run := s.fireSchedule(ctx, sc, slotKey(slot), now, false)
		sc.Runs++
		record(run)
		if limitReached() {
			end("max runs reached")
			return
		}
	}

	// Every slot from the cursor up to now (and the end time).
	var due []time.Time
	for cur := sc.LastSlot; len(due) < maxDueSlots; {
		n, ok := spec.Next(cur)
		if !ok || n.After(now) || (!sc.EndAt.IsZero() && n.After(sc.EndAt)) {
			break
		}
		due = append(due, n)
		cur = n
	}
	if len(due) > 0 {
		last := due[len(due)-1]
		key := slotKey(last)
		if limitReached() {
			end("max runs reached")
			return
		}
		switch jobID, wfID, started := s.scheduledRunOf(sc.ID, key); {
		case started:
			// Started before a crash that lost the record of it: link it, don't repeat it.
			sc.Runs++
			record(domain.ScheduleRun{Slot: key, At: now, Outcome: domain.RunStarted, Reason: "found already started after a restart", Job: jobID, Workflow: wfID})
		default:
			onTime := now.Sub(last) <= scheduleGrace
			skippedBefore := len(due) - 1
			if !onTime {
				skippedBefore = len(due)
			}
			if skippedBefore > 0 && !(!onTime && sc.Missed == domain.MissedRunOnce) {
				record(domain.ScheduleRun{Slot: key, At: now, Outcome: domain.RunMissed,
					Reason: fmt.Sprintf("%d slot(s) passed while the manager was off or the clock jumped", skippedBefore)})
			}
			switch {
			case !onTime && sc.Missed == domain.MissedSkip:
				// dropped, as recorded
			case s.scheduleRunning(sc.ID) && sc.Overlap == domain.OverlapSkip:
				record(domain.ScheduleRun{Slot: key, At: now, Outcome: domain.RunSkipped, Reason: "the run before it is still going"})
			case s.scheduleRunning(sc.ID) && sc.Overlap == domain.OverlapQueue:
				if sc.QueuedSlot.IsZero() {
					sc.QueuedSlot = last
					record(domain.ScheduleRun{Slot: key, At: now, Outcome: domain.RunQueued, Reason: "waiting for the run before it to finish"})
				} else {
					record(domain.ScheduleRun{Slot: key, At: now, Outcome: domain.RunSkipped, Reason: "a run is already waiting behind the one going"})
				}
			default:
				run := s.fireSchedule(ctx, sc, key, now, false)
				if !onTime {
					run.Reason = strings.TrimSpace(run.Reason + fmt.Sprintf(" (catch-up for %d missed slot(s))", len(due)))
				}
				sc.Runs++
				record(run)
			}
		}
		sc.LastSlot = last
		changed = true
		if limitReached() {
			end("max runs reached")
			return
		}
	}
	// Nothing more to come before the end time?
	if next, ok := spec.Next(sc.LastSlot); !ok || (!sc.EndAt.IsZero() && next.After(sc.EndAt)) {
		if sc.QueuedSlot.IsZero() {
			end("end time reached")
		}
	}
}

// scheduleLiveArtifacts keeps the files a schedule will submit alive for
// as long as the schedule exists, paused or not: the garbage collector
// and artifact deletion both count them as in use.
func (s *Server) scheduleLiveArtifacts(live map[string]bool) {
	t := s.scheduleTbl()
	if t == nil {
		return
	}
	for _, sc := range t.list() {
		if sc.State == domain.ScheduleEnded {
			continue
		}
		if sc.Workflow != nil {
			for _, in := range sc.Workflow.Inputs {
				live[in.SHA256] = true
			}
		}
		if sc.Job != nil {
			specs := append([]domain.TaskSpec{}, sc.Job.Tasks...)
			if sc.Job.Reduce != nil {
				specs = append(specs, *sc.Job.Reduce)
			}
			for _, ts := range specs {
				for _, in := range ts.Inputs {
					live[in.SHA256] = true
				}
			}
		}
	}
}
