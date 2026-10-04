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
}

// A workflow was submitted, moved to its next stage, or finished (data:
// workflowId, and stage or state).
const (
	EventWorkflowSubmitted EventType = "workflow.submitted"
	EventWorkflowStage     EventType = "workflow.stage"
	EventWorkflowFinished  EventType = "workflow.finished"
)
