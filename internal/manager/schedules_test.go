package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// schedClock is a hand-set clock for the scheduler.
type schedClock struct{ t time.Time }

func (c *schedClock) now() time.Time { return c.t }
func (c *schedClock) set(t time.Time) { c.t = t }

func scheduleTestServer(t *testing.T) (*Server, *schedClock) {
	t.Helper()
	s, _ := workflowTestServer(t)
	s.schedules = newScheduleTable()
	s.auditLog = newAuditRecorder()
	clk := &schedClock{t: time.Date(2026, 5, 1, 10, 0, 30, 0, time.UTC)}
	s.schedules.now = clk.now
	return s, clk
}

func identityJob() *domain.JobTemplate {
	return &domain.JobTemplate{Tasks: []domain.TaskSpec{{Capability: "system.identity"}}}
}

func newSchedule(t *testing.T, s *Server, req ScheduleRequest) domain.Schedule {
	t.Helper()
	if req.Spec == "" {
		req.Spec = "* * * * *"
	}
	if req.Job == nil && req.Workflow == nil {
		req.Job = identityJob()
	}
	sc, err := s.CreateSchedule(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func jobsOf(s *Server, id domain.ScheduleID) []domain.Job {
	var out []domain.Job
	for _, j := range s.jobs.list() {
		if j.Schedule == string(id) {
			out = append(out, j)
		}
	}
	return out
}

func stopJobs(s *Server, id domain.ScheduleID) {
	for _, j := range jobsOf(s, id) {
		s.CancelJob(context.Background(), j.ID)
	}
}

func current(s *Server, id domain.ScheduleID) domain.Schedule {
	sc, _ := s.schedules.get(id)
	return sc
}

func TestScheduleFiresOncePerSlot(t *testing.T) {
	s, clk := scheduleTestServer(t)
	sc := newSchedule(t, s, ScheduleRequest{Name: "tick", Overlap: domain.OverlapAllow})
	s.advanceSchedules(t.Context())
	if n := len(jobsOf(s, sc.ID)); n != 0 {
		t.Fatalf("fired before its slot: %d", n)
	}
	clk.set(time.Date(2026, 5, 1, 10, 1, 5, 0, time.UTC))
	for i := 0; i < 5; i++ { // many passes within the same slot
		s.advanceSchedules(t.Context())
	}
	jobs := jobsOf(s, sc.ID)
	if len(jobs) != 1 || jobs[0].Slot != "2026-05-01T10:01:00Z" || jobs[0].Name != "tick" {
		t.Fatalf("jobs %+v", jobs)
	}
	clk.set(time.Date(2026, 5, 1, 10, 2, 1, 0, time.UTC))
	s.advanceSchedules(t.Context())
	got := current(s, sc.ID)
	if len(jobsOf(s, sc.ID)) != 2 || got.Runs != 2 || len(got.History) != 2 || got.History[0].Job == "" {
		t.Fatalf("after two slots: runs %d history %+v", got.Runs, got.History)
	}
}

func TestScheduleMissedPolicies(t *testing.T) {
	for _, policy := range []string{domain.MissedSkip, domain.MissedRunOnce} {
		t.Run(policy, func(t *testing.T) {
			s, clk := scheduleTestServer(t)
			sc := newSchedule(t, s, ScheduleRequest{Spec: "*/5 * * * *", Missed: policy, Overlap: domain.OverlapAllow})
			clk.set(time.Date(2026, 5, 1, 10, 19, 30, 0, time.UTC)) // the manager was off for three slots (10:05, 10:10, 10:15)
			s.advanceSchedules(t.Context())
			n := len(jobsOf(s, sc.ID))
			h := current(s, sc.ID).History
			if policy == domain.MissedSkip && (n != 0 || len(h) != 1 || h[0].Outcome != domain.RunMissed) {
				t.Fatalf("skip: %d jobs, history %+v", n, h)
			}
			if policy == domain.MissedRunOnce && (n != 1 || h[len(h)-1].Outcome != domain.RunStarted || !strings.Contains(h[len(h)-1].Reason, "catch-up for 3")) {
				t.Fatalf("run-once: %d jobs, history %+v", n, h)
			}
			s.advanceSchedules(t.Context())
			if len(jobsOf(s, sc.ID)) != n {
				t.Fatal("the missed slots were run again")
			}
			// The next slot on time runs normally.
			clk.set(time.Date(2026, 5, 1, 10, 20, 2, 0, time.UTC))
			s.advanceSchedules(t.Context())
			if len(jobsOf(s, sc.ID)) != n+1 {
				t.Fatalf("next slot: %d jobs", len(jobsOf(s, sc.ID)))
			}
		})
	}
}

func TestScheduleOverlapPolicies(t *testing.T) {
	s, clk := scheduleTestServer(t)
	skip := newSchedule(t, s, ScheduleRequest{Overlap: domain.OverlapSkip})
	queue := newSchedule(t, s, ScheduleRequest{Overlap: domain.OverlapQueue})
	allow := newSchedule(t, s, ScheduleRequest{Overlap: domain.OverlapAllow})
	for _, at := range []time.Time{time.Date(2026, 5, 1, 10, 1, 1, 0, time.UTC), time.Date(2026, 5, 1, 10, 2, 1, 0, time.UTC), time.Date(2026, 5, 1, 10, 3, 1, 0, time.UTC)} {
		clk.set(at)
		s.advanceSchedules(t.Context())
	}
	// The first job of each is still running through slots 2 and 3.
	if n := len(jobsOf(s, skip.ID)); n != 1 {
		t.Fatalf("skip: %d jobs", n)
	}
	if h := current(s, skip.ID).History; len(h) != 3 || h[1].Outcome != domain.RunSkipped {
		t.Fatalf("skip history %+v", h)
	}
	if n := len(jobsOf(s, allow.ID)); n != 3 {
		t.Fatalf("allow: %d jobs", n)
	}
	q := current(s, queue.ID)
	if len(jobsOf(s, queue.ID)) != 1 || q.QueuedSlot.IsZero() || q.History[1].Outcome != domain.RunQueued || q.History[2].Outcome != domain.RunSkipped {
		t.Fatalf("queue: %d jobs, queued %v, history %+v", len(jobsOf(s, queue.ID)), q.QueuedSlot, q.History)
	}
	// Once the running job ends, the queued slot runs (once).
	stopJobs(s, queue.ID)
	s.advanceSchedules(t.Context())
	s.advanceSchedules(t.Context())
	jobs := jobsOf(s, queue.ID)
	slots := map[string]bool{}
	for _, j := range jobs {
		slots[j.Slot] = true
	}
	if len(jobs) != 2 || !slots["2026-05-01T10:02:00Z"] || !current(s, queue.ID).QueuedSlot.IsZero() {
		t.Fatalf("queued run: %+v", jobs)
	}
}

// With no device ready (agents still reconnecting after a restart) an
// on-time slot waits for the next pass instead of being used up.
func TestScheduleWaitsForADevice(t *testing.T) {
	s, clk := scheduleTestServer(t)
	sc := newSchedule(t, s, ScheduleRequest{Overlap: domain.OverlapAllow})
	s.Registry.SetState("n1", domain.NodeReconnecting)
	clk.set(time.Date(2026, 5, 1, 10, 1, 5, 0, time.UTC))
	s.advanceSchedules(t.Context())
	s.advanceSchedules(t.Context())
	if got := current(s, sc.ID); len(got.History) != 0 || got.Runs != 0 || len(jobsOf(s, sc.ID)) != 0 {
		t.Fatalf("used the slot up with no device: %+v", got)
	}
	s.Registry.SetState("n1", domain.NodeReady)
	clk.set(time.Date(2026, 5, 1, 10, 1, 20, 0, time.UTC))
	s.advanceSchedules(t.Context())
	if got := current(s, sc.ID); len(jobsOf(s, sc.ID)) != 1 || got.Runs != 1 {
		t.Fatalf("did not run once a device was ready: %+v", got)
	}
}

func TestScheduleMaxRunsAndEndTime(t *testing.T) {
	s, clk := scheduleTestServer(t)
	two := newSchedule(t, s, ScheduleRequest{MaxRuns: 2, Overlap: domain.OverlapAllow})
	end := newSchedule(t, s, ScheduleRequest{EndAt: time.Date(2026, 5, 1, 10, 2, 30, 0, time.UTC), Overlap: domain.OverlapAllow})
	for m := 1; m <= 6; m++ {
		clk.set(time.Date(2026, 5, 1, 10, m, 1, 0, time.UTC))
		s.advanceSchedules(t.Context())
	}
	if got := current(s, two.ID); got.State != domain.ScheduleEnded || got.Runs != 2 || len(jobsOf(s, two.ID)) != 2 {
		t.Fatalf("max runs: %s runs %d jobs %d", got.State, got.Runs, len(jobsOf(s, two.ID)))
	}
	if got := current(s, end.ID); got.State != domain.ScheduleEnded || len(jobsOf(s, end.ID)) != 2 {
		t.Fatalf("end time: %s jobs %d (slots 10:01 and 10:02 only)", got.State, len(jobsOf(s, end.ID)))
	}
}

// A clock that goes backwards must not repeat a run; one that goes
// forwards runs the latest slot once.
func TestScheduleClockJumps(t *testing.T) {
	s, clk := scheduleTestServer(t)
	sc := newSchedule(t, s, ScheduleRequest{Overlap: domain.OverlapAllow})
	clk.set(time.Date(2026, 5, 1, 10, 5, 1, 0, time.UTC))
	s.advanceSchedules(t.Context())
	base := len(jobsOf(s, sc.ID))
	clk.set(time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)) // an hour back
	for i := 0; i < 3; i++ {
		s.advanceSchedules(t.Context())
	}
	if len(jobsOf(s, sc.ID)) != base {
		t.Fatalf("ran again after the clock went back: %d -> %d", base, len(jobsOf(s, sc.ID)))
	}
	clk.set(time.Date(2026, 5, 1, 10, 6, 1, 0, time.UTC)) // caught up past the next slot
	s.advanceSchedules(t.Context())
	if len(jobsOf(s, sc.ID)) != base+1 {
		t.Fatalf("not resumed after the clock caught up: %d jobs", len(jobsOf(s, sc.ID)))
	}
	seen := map[string]bool{}
	for _, j := range jobsOf(s, sc.ID) {
		if seen[j.Slot] {
			t.Fatalf("slot %s ran twice", j.Slot)
		}
		seen[j.Slot] = true
	}
}

// A crash after the job was recorded but before the schedule's record:
// the next pass links the job it finds instead of submitting another.
func TestScheduleDoesNotRepeatARunLostToACrash(t *testing.T) {
	s, clk := scheduleTestServer(t)
	sc := newSchedule(t, s, ScheduleRequest{Overlap: domain.OverlapAllow})
	clk.set(time.Date(2026, 5, 1, 10, 1, 2, 0, time.UTC))
	s.advanceSchedules(t.Context())
	if len(jobsOf(s, sc.ID)) != 1 {
		t.Fatal("setup")
	}
	s.schedules.put(sc) // the record as it was before that pass
	s.advanceSchedules(t.Context())
	s.advanceSchedules(t.Context())
	got := current(s, sc.ID)
	if len(jobsOf(s, sc.ID)) != 1 || got.Runs != 1 || len(got.History) != 1 || got.History[0].Job == "" {
		t.Fatalf("after the crash: %d jobs, runs %d, history %+v", len(jobsOf(s, sc.ID)), got.Runs, got.History)
	}
}

func TestScheduleRunFailsClearlyWhenPolicyChanged(t *testing.T) {
	s, clk := scheduleTestServer(t)
	sc := newSchedule(t, s, ScheduleRequest{})
	p := domain.PermissivePolicy()
	p.Types = map[domain.CapabilityName]domain.TypePolicy{"system.identity": {Enabled: false}}
	s.policy = &policyStore{}
	s.policy.set(p)
	clk.set(time.Date(2026, 5, 1, 10, 1, 2, 0, time.UTC))
	s.advanceSchedules(t.Context())
	got := current(s, sc.ID)
	if len(jobsOf(s, sc.ID)) != 0 || len(got.History) != 1 || got.History[0].Outcome != domain.RunFailed || !strings.Contains(got.History[0].Reason, "disabled") || got.Runs != 1 {
		t.Fatalf("history %+v runs %d", got.History, got.Runs)
	}
	// And creating one for a disabled type is refused outright.
	if _, err := s.CreateSchedule(t.Context(), ScheduleRequest{Spec: "@hourly", Job: identityJob()}); err == nil {
		t.Fatal("a schedule for a disabled type was created")
	}
}

func TestSchedulePauseResumeRunNowDelete(t *testing.T) {
	s, clk := scheduleTestServer(t)
	sc := newSchedule(t, s, ScheduleRequest{Overlap: domain.OverlapAllow})
	if _, err := s.PauseSchedule(sc.ID); err != nil {
		t.Fatal(err)
	}
	clk.set(time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)) // half an hour paused
	s.advanceSchedules(t.Context())
	if len(jobsOf(s, sc.ID)) != 0 {
		t.Fatal("a paused schedule fired")
	}
	run, err := s.RunScheduleNow(t.Context(), sc.ID)
	if err != nil || run.Outcome != domain.RunStarted || !run.Manual {
		t.Fatalf("run now: %+v %v", run, err)
	}
	if got := current(s, sc.ID); got.Runs != 0 || got.State != domain.SchedulePaused {
		t.Fatalf("a manual run counted or changed state: %+v", got)
	}
	if _, err := s.ResumeSchedule(sc.ID); err != nil {
		t.Fatal(err)
	}
	s.advanceSchedules(t.Context())
	if n := len(jobsOf(s, sc.ID)); n != 1 { // only the manual one: the paused time is not caught up
		t.Fatalf("resume caught up the pause: %d jobs", n)
	}
	if _, err := s.ResumeSchedule(sc.ID); err == nil {
		t.Fatal("resumed an active schedule")
	}
	if err := s.DeleteSchedule(sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunScheduleNow(t.Context(), sc.ID); err == nil {
		t.Fatal("ran a deleted schedule")
	}
}

func TestScheduleKeepsItsFilesFromGC(t *testing.T) {
	s, _ := scheduleTestServer(t)
	files := photoFiles(t, s.cfg.Artifacts)
	sc := newSchedule(t, s, ScheduleRequest{Spec: "@daily", Workflow: &domain.WorkflowTemplate{Inputs: files, Stages: []domain.WorkflowStage{stage("file.hash", "", nil)}}})
	live := map[string]bool{}
	s.scheduleLiveArtifacts(live)
	if !live[files[0].SHA256] || !live[files[1].SHA256] {
		t.Fatalf("a schedule's files are not protected: %v", live)
	}
	if _, err := s.PauseSchedule(sc.ID); err != nil {
		t.Fatal(err)
	}
	live = map[string]bool{}
	s.scheduleLiveArtifacts(live)
	if !live[files[0].SHA256] {
		t.Fatal("paused schedule's files unprotected")
	}
	s.DeleteSchedule(sc.ID)
	live = map[string]bool{}
	s.scheduleLiveArtifacts(live)
	if live[files[0].SHA256] {
		t.Fatal("deleted schedule still protects files")
	}
}

func TestScheduleValidation(t *testing.T) {
	s, _ := scheduleTestServer(t)
	cases := map[string]ScheduleRequest{
		"bad cron":      {Spec: "61 * * * *", Job: identityJob()},
		"never":         {Spec: "0 0 30 2 *", Job: identityJob()},
		"bad zone":      {Spec: "@daily", Timezone: "Mars/Base", Job: identityJob()},
		"both":          {Spec: "@daily", Job: identityJob(), Workflow: &domain.WorkflowTemplate{}},
		"neither":       {Spec: "@daily"},
		"bad missed":    {Spec: "@daily", Missed: "maybe", Job: identityJob()},
		"bad overlap":   {Spec: "@daily", Overlap: "pile", Job: identityJob()},
		"past end":      {Spec: "@daily", EndAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Job: identityJob()},
		"bad work":      {Spec: "@daily", Job: &domain.JobTemplate{Tasks: []domain.TaskSpec{{Capability: "cpu.burn", Params: map[string]string{"seconds": "999999"}}}}},
		"bad workflow":  {Spec: "@daily", Workflow: &domain.WorkflowTemplate{Stages: []domain.WorkflowStage{stage("file.hash", "", nil)}}},
	}
	for name, req := range cases {
		if _, err := s.CreateSchedule(t.Context(), req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Across the spring-forward gap a 02:30 New York job runs once.
func TestScheduleAcrossDST(t *testing.T) {
	s, clk := scheduleTestServer(t)
	clk.set(time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC)) // 01:00 EST
	sc := newSchedule(t, s, ScheduleRequest{Spec: "30 2 * * *", Timezone: "America/New_York", Overlap: domain.OverlapAllow})
	for m := 0; m <= 360; m++ {
		clk.set(time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute).Add(2 * time.Second))
		s.advanceSchedules(t.Context())
	}
	jobs := jobsOf(s, sc.ID)
	if len(jobs) != 1 || jobs[0].Slot != "2026-03-08T07:30:00Z" { // 03:30 EDT
		t.Fatalf("jobs across the gap: %+v", jobs)
	}
}
