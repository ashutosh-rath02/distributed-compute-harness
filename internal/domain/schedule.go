package domain

import "time"

// ScheduleID identifies a schedule (manager/schedules.go).
type ScheduleID string

// ScheduleState: ACTIVE fires on its slots, PAUSED waits (a resume does
// not catch up the pause), ENDED has reached its max runs or end time.
type ScheduleState string

const (
	ScheduleActive ScheduleState = "ACTIVE"
	SchedulePaused ScheduleState = "PAUSED"
	ScheduleEnded  ScheduleState = "ENDED"
)

// What to do about slots that passed while the manager was off (or the
// clock jumped over them), and about a slot that arrives while the run
// before it is still going.
const (
	MissedSkip    = "skip"     // missed slots are dropped
	MissedRunOnce = "run-once" // one catch-up run, however many were missed

	OverlapSkip  = "skip"  // the new slot is dropped
	OverlapQueue = "queue" // one run waits for the old to finish
	OverlapAllow = "allow" // both run
)

// Schedule limits.
const (
	MaxSchedules       = 200
	MaxScheduleHistory = 50
)

// JobTemplate is what a schedule submits as a batch job each time.
type JobTemplate struct {
	Name        string     `json:"name,omitempty"`
	Tasks       []TaskSpec `json:"tasks"`
	Reduce      *TaskSpec  `json:"reduce,omitempty"`
	MaxAttempts int        `json:"maxAttempts,omitempty"`
	Priority    Priority   `json:"priority,omitempty"`
}

// WorkflowTemplate is what a schedule submits as a workflow each time.
type WorkflowTemplate struct {
	Name        string          `json:"name,omitempty"`
	Inputs      []ArtifactRef   `json:"inputs,omitempty"`
	Stages      []WorkflowStage `json:"stages"`
	MaxAttempts int             `json:"maxAttempts,omitempty"`
}

// Run outcomes recorded in a schedule's history.
const (
	RunStarted = "started" // submitted; the linked job or workflow says how it went
	RunFailed  = "failed"  // could not be submitted (Reason says why)
	RunSkipped = "skipped" // dropped by the overlap policy
	RunMissed  = "missed"  // slots that passed while the manager was off, dropped
	RunQueued  = "queued"  // waiting for the run before it to finish
)

// ScheduleRun is one entry of a schedule's history.
type ScheduleRun struct {
	// Slot is the scheduled instant (RFC 3339, UTC) this entry is about;
	// "manual-..." for a run-now.
	Slot     string     `json:"slot"`
	At       time.Time  `json:"at"`
	Outcome  string     `json:"outcome"`
	Reason   string     `json:"reason,omitempty"`
	Manual   bool       `json:"manual,omitempty"`
	Job      JobID      `json:"job,omitempty"`
	Workflow WorkflowID `json:"workflow,omitempty"`
}

// Schedule is recurring work: a job or a workflow submitted on a cron
// expression, an @macro or "every 15m". Everything needed to carry on
// after a restart is in this record: the last slot dealt with, the run
// count, the run waiting under overlap "queue", and the history.
type Schedule struct {
	ID       ScheduleID `json:"id"`
	Name     string     `json:"name,omitempty"`
	Spec     string     `json:"spec"`     // as written
	Timezone string     `json:"timezone"` // IANA name; "UTC" if none given
	// Anchor is when the schedule was made: "every" intervals count from it.
	Anchor   time.Time         `json:"anchor"`
	Job      *JobTemplate      `json:"job,omitempty"`
	Workflow *WorkflowTemplate `json:"workflow,omitempty"`
	Missed   string            `json:"missed"`
	Overlap  string            `json:"overlap"`
	// MaxRuns ends the schedule after that many runs (0 = no limit); EndAt
	// stops it from firing slots after that instant (zero = never).
	MaxRuns int       `json:"maxRuns,omitempty"`
	EndAt   time.Time `json:"endAt,omitempty"`

	State     ScheduleState `json:"state"`
	CreatedAt time.Time     `json:"createdAt"`
	// LastSlot is the latest slot dealt with (run, skipped or dropped): no
	// slot at or before it ever fires, whatever the clock does.
	LastSlot time.Time `json:"lastSlot"`
	// Runs counts the slots that were run (or tried), not manual runs.
	Runs int `json:"runs"`
	// QueuedSlot is the slot waiting under overlap "queue" (zero = none).
	QueuedSlot time.Time     `json:"queuedSlot,omitempty"`
	History    []ScheduleRun `json:"history,omitempty"`
}

// Kind is "job" or "workflow".
func (s Schedule) Kind() string {
	if s.Workflow != nil {
		return "workflow"
	}
	return "job"
}
