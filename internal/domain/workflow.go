package domain

import "time"

// WorkflowID identifies a workflow (roadmap item 15).
type WorkflowID string

// MaxWorkflowStages bounds a workflow's steps.
const MaxWorkflowStages = 8

// How a workflow stage runs over the files it is given: the workflow's
// own files for the first stage, the stage before's results after that.
const (
	StageOnce    = "once"    // one run with every file
	StagePerFile = "perFile" // one run per file, spread over the fleet
	StageParts   = "parts"   // one run per part (catalog Type.Parts)
)

// WorkflowStep is a catalog task type and its parameters.
type WorkflowStep struct {
	Type   CapabilityName    `json:"type"`
	Params map[string]string `json:"params,omitempty"`
}

// WorkflowStage is one step of a workflow: an ordinary job, built from
// this template once the stage before it has completed. Combine is an
// optional fan-in step over the stage's results (the job's reduce).
type WorkflowStage struct {
	Name string `json:"name,omitempty"`
	WorkflowStep
	Mode    string        `json:"mode,omitempty"`
	Parts   int           `json:"parts,omitempty"`
	Combine *WorkflowStep `json:"combine,omitempty"`
	// When runs the stage only if the condition holds (condition.go in the
	// manager); empty = always. OnFailure is "stop" (the default),
	// "continue" or "retry:N". TimeoutSeconds limits each run of each task;
	// MaxAttempts overrides the workflow's attempts per task for the stage.
	When           string `json:"when,omitempty"`
	OnFailure      string `json:"onFailure,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	MaxAttempts    int    `json:"maxAttempts,omitempty"`
}

// StageMark records a stage that failed without ever having a job (its
// job could not be submitted, or it had nothing to work on) and whose
// failure the workflow tolerates (onFailure continue). Every other stage
// state is derived from the stages' jobs.
type StageMark struct {
	Stage  int    `json:"stage"` // from 0
	Reason string `json:"reason"`
}

// Workflow is an ordered list of stages, each consuming the results of
// the one before it. Like a job's attempts, its stages' jobs carry the
// workflow's ID (Job.Workflow, Job.Stage) and its progress is derived
// from them; only the request and the final outcome are stored here.
type Workflow struct {
	ID   WorkflowID `json:"id"`
	Name string     `json:"name,omitempty"`
	// Inputs are the first stage's files.
	Inputs      []ArtifactRef   `json:"inputs,omitempty"`
	Stages      []WorkflowStage `json:"stages"`
	MaxAttempts int             `json:"maxAttempts"`
	State       JobState        `json:"state"`
	Error       string          `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	FinishedAt  time.Time       `json:"finishedAt,omitempty"`
	// Marks: see StageMark.
	Marks []StageMark `json:"marks,omitempty"`
	// Schedule and Slot say which schedule run started it (schedules.go);
	// empty for one submitted by hand.
	Schedule string `json:"schedule,omitempty"`
	Slot     string `json:"slot,omitempty"`
}

// A workflow was submitted, moved to its next stage, or finished (data:
// workflowId, and stage or state).
const (
	EventWorkflowSubmitted EventType = "workflow.submitted"
	EventWorkflowStage     EventType = "workflow.stage"
	EventWorkflowFinished  EventType = "workflow.finished"
)
