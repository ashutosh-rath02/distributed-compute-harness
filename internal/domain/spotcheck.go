package domain

import "time"

// SpotCheckState is where a spot check stands.
type SpotCheckState string

const (
	SpotChecking SpotCheckState = "checking"
	SpotMatched  SpotCheckState = "matched"
	// SpotMismatch: the results differed and a third device settled it —
	// Suspects names the odd one out.
	SpotMismatch SpotCheckState = "mismatch"
	// SpotUnresolved: the results differed and nothing could say which was
	// right (no third device, or no two agreed). Nobody is marked.
	SpotUnresolved SpotCheckState = "unresolved"
	// SpotSkipped: no comparison was made (Detail says why).
	SpotSkipped SpotCheckState = "skipped"
)

// SpotCheck is one completed job task run again on other devices and
// compared (roadmap-after-9 item 20). Results are compared by the hashes
// the manager computed as it received each output file — never by what
// an agent claims — and a check is never a job attempt: it can't change
// the job's state or outputs.
type SpotCheck struct {
	Job        JobID          `json:"job"`
	Task       string         `json:"task"`
	Capability CapabilityName `json:"capability"`
	// Original is the task's completed attempt, Node where it ran, and
	// Outputs the files the manager received from it.
	Original WorkloadID    `json:"original"`
	Node     NodeID        `json:"node"`
	Outputs  []ArtifactRef `json:"outputs"`
	// Runs are the re-runs, oldest first: the check, then a tiebreak.
	Runs  []SpotCheckRun `json:"runs,omitempty"`
	State SpotCheckState `json:"state"`
	// Detail says what happened in plain words.
	Detail string `json:"detail,omitempty"`
	// Suspects are the devices whose result was the odd one out.
	Suspects []NodeID `json:"suspects,omitempty"`
	// Audited: this check wrote its job's node.result-mismatch entry
	// (at most one per job and device, so a device that is wrong a
	// thousand times can't flood the security log).
	Audited    bool      `json:"audited,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// EventJobSpotCheck: a job's spot check started or reached a verdict
// (data: jobId, task, state).
const EventJobSpotCheck EventType = "job.spot-check"

// SpotCheckRun is one re-run: an ordinary workload (no job, no attempt
// number) kept off every device that already ran this task.
type SpotCheckRun struct {
	Workload WorkloadID    `json:"workload"`
	Node     NodeID        `json:"node,omitempty"`
	State    WorkloadState `json:"state"`
	Outputs  []ArtifactRef `json:"outputs,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// SuspectMark flags a device whose results disagreed with other
// devices' in spot checks. It only informs the operator: nothing acts on
// it automatically (no revocation, no placement change), and the
// operator clears it.
type SuspectMark struct {
	Since time.Time `json:"since"`
	Last  time.Time `json:"last"`
	// Count is how many spot checks found this device the odd one out.
	Count int `json:"count"`
	// The latest such check, and what it found, in plain words.
	Job    JobID  `json:"job"`
	Task   string `json:"task"`
	Reason string `json:"reason"`
}
