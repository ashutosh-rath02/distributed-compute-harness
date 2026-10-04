package domain

import (
	"fmt"
	"time"
)

// JobID identifies a batch job (roadmap item 6).
type JobID string

// JobState is a job's overall outcome.
type JobState string

const (
	JobRunning   JobState = "RUNNING"
	JobCompleted JobState = "COMPLETED"
	JobFailed    JobState = "FAILED"
	JobCanceled  JobState = "CANCELED"
)

// Job limits.
const (
	MaxJobTasks       = 1000
	DefaultJobRetries = 3 // attempts per task, the first run included
	MaxJobAttempts    = 10
)

// TaskSpec is one unit of work in a job: what a single workload
// submission would carry. Name is only a display label.
type TaskSpec struct {
	Name   string `json:"name,omitempty"`
	Target NodeID `json:"target,omitempty"`
	// Capability is a catalog task type with its Params (outputs then come
	// from the type), or empty for a raw command.
	Capability   CapabilityName       `json:"capability,omitempty"`
	Params       map[string]string    `json:"params,omitempty"`
	Command      string               `json:"command,omitempty"`
	Args         []string             `json:"args,omitempty"`
	Inputs       []ArtifactRef        `json:"inputs,omitempty"`
	Outputs      []string             `json:"outputs,omitempty"`
	Requirements ResourceRequirements `json:"requirements,omitempty"`
}

// Job is one request fanned out as many tasks (each run as ordinary
// workloads, one per attempt) and optionally fanned back in by a Reduce
// task that receives every task's outputs. Only the request and the
// overall outcome are stored here: which attempt each task is on, how it
// went, and its outputs are derived from the attempts' own workload
// records, so the two can never disagree after a crash.
type Job struct {
	ID          JobID      `json:"id"`
	Name        string     `json:"name,omitempty"`
	Tasks       []TaskSpec `json:"tasks"`
	Reduce      *TaskSpec  `json:"reduce,omitempty"`
	MaxAttempts int        `json:"maxAttempts"`
	State       JobState   `json:"state"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	FinishedAt  time.Time  `json:"finishedAt,omitempty"`
	// Priority is every attempt's queue priority (priority.go); empty =
	// normal.
	Priority Priority `json:"priority,omitempty"`
	// Workflow and Stage (from 0) are set on a workflow stage's job
	// (workflow.go): the workflow's progress is derived from them.
	Workflow WorkflowID `json:"workflow,omitempty"`
	Stage    int        `json:"stage,omitempty"`
	// SpotCheckPercent is the policy's spot-check share when the job was
	// submitted (spot checks apply to jobs submitted while they are on).
	SpotCheckPercent int `json:"spotCheckPercent,omitempty"`
}

// ReduceTask is the task key of a job's fan-in step.
const ReduceTask = "reduce"

// TaskKey is task i's key: the Task field of its attempts and the
// directory its outputs land in for the reduce ("parts/0007/...").
func TaskKey(i int) string { return fmt.Sprintf("%04d", i) }

// ReducePartName is where task key's output name lands in the reduce
// task's working directory.
func ReducePartName(key, output string) string { return "parts/" + key + "/" + output }
