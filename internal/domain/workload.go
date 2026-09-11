package domain

import "time"

// WorkloadID identifies a single workload submission, distinct from
// CommandResult's CommandID correlation — a workload is long-lived (it has
// a running phase between assignment and completion) where a Command is a
// single request/reply.
type WorkloadID string

// WorkloadState is a workload's position in its execution lifecycle.
type WorkloadState string

const (
	WorkloadPending   WorkloadState = "PENDING"
	WorkloadRunning   WorkloadState = "RUNNING"
	WorkloadCompleted WorkloadState = "COMPLETED"
	WorkloadFailed    WorkloadState = "FAILED"
	WorkloadCanceled  WorkloadState = "CANCELED"
	// WorkloadUnknown marks a workload that was RUNNING when the manager
	// last knew about it but whose true outcome is unknown after a manager
	// restart (v1 explicitly does not attempt reconciliation with the
	// agent's actual state — that is v3 scope).
	WorkloadUnknown WorkloadState = "UNKNOWN"
)

// Workload is a single subprocess execution request. Command/Args are
// argv-form (never a shell string): the agent execs Command with Args
// directly, so there is no shell to inject into and no cmd.exe-vs-sh
// ambiguity about how the string would be split.
type Workload struct {
	ID      WorkloadID `json:"id"`
	Target  NodeID     `json:"target"`
	Command string     `json:"command"`
	Args    []string   `json:"args,omitempty"`
	// Pinned records whether the original submission named an explicit
	// Target (true) or left placement to auto-selection (false). Target
	// itself gets overwritten with whatever node was actually resolved to
	// on submission (and again on each restart), so this is the only place
	// that original intent survives — without it, a restart couldn't tell
	// "stay on this exact node" from "any eligible node is fine" once
	// Target already holds a concrete resolved value either way.
	Pinned bool `json:"pinned,omitempty"`
	// Requirements records what placement constraints, if any, this
	// workload was submitted with — kept on the workload itself (not just
	// the API request) so a persisted/historical record is self-explaining
	// about why it landed where it did.
	Requirements ResourceRequirements `json:"requirements,omitempty"`
	// RestartPolicy is immutable once submitted, unlike RestartState (see
	// restart_policy.go), which is manager-only bookkeeping that changes
	// over the workload's lifetime.
	RestartPolicy RestartPolicy `json:"restartPolicy,omitempty"`
	// Capability names what this workload actually invokes on the target
	// node — v4's generalization beyond raw command execution (baseline
	// architecture doc §3: "Workload... capability invocation"). Empty
	// means CapabilitySystemExecute (Command/Args below), preserving every
	// pre-v4 workload's exact meaning. Use EffectiveCapability, not this
	// field directly, wherever the empty case must resolve to a concrete
	// value (placement, dispatch) — a workload persisted before this field
	// existed, or a restart of one, must not be treated as requesting a
	// capability literally named "".
	Capability CapabilityName `json:"capability,omitempty"`
	// Params carries capability-specific input that isn't Command/Args
	// shaped (e.g. filesystem.read's "path"), mirroring Command.Params'
	// existing shape (command.go) — same vocabulary, no new pattern.
	// Command/Args stay separate, first-class fields rather than folding
	// into this generic bag: Args is an ordered list that doesn't fit a
	// string-keyed map, and system.execute's existing wire/CLI shape has
	// no reason to change.
	Params map[string]string `json:"params,omitempty"`
}

// EffectiveCapability returns w.Capability, or CapabilitySystemExecute if
// it's empty. Read sites that make a placement or dispatch decision (not
// just display) must call this instead of reading Capability directly —
// see the field's own doc comment for why the empty case needs resolving.
func (w Workload) EffectiveCapability() CapabilityName {
	if w.Capability == "" {
		return CapabilitySystemExecute
	}
	return w.Capability
}

// outputCap bounds how much of a workload's stdout/stderr is retained and
// sent back to the manager. A runaway or chatty process must not be able to
// balloon agent memory, the WORKLOAD_STATUS payload, or the manager's
// persistent record.
const OutputCapBytes = 64 * 1024

// WorkloadStatus is a workload's current or final status, reported by the
// agent and stored by the manager.
type WorkloadStatus struct {
	ID     WorkloadID    `json:"id"`
	Target NodeID        `json:"target"`
	State  WorkloadState `json:"state"`
	// Stdout is the capability's primary textual output — process stdout
	// for system.execute, but reused as-is for a non-exec capability's
	// result (e.g. filesystem.read's file content) rather than adding a
	// separate result field per capability.
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	ExitCode   int       `json:"exitCode,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// PersistedWorkload is what survives a manager restart for one workload:
// the original request plus its last-known status. It lives in domain (not
// the manager or persistent packages) so both can depend on this shared
// shape without importing each other — the same reason Manifest is used
// directly as the persisted/passed form for nodes.
type PersistedWorkload struct {
	Workload Workload       `json:"workload"`
	Status   WorkloadStatus `json:"status"`
	// Restart is manager-only bookkeeping (see RestartState) persisted
	// alongside the workload so restart count/backoff survive a manager
	// restart instead of resetting to zero — without this, a crash-looping
	// workload would get hammered at full reconcile-tick frequency right
	// when the fleet is least ready for it. Old persisted records (before
	// v3) decode with this as its zero value; no migration needed.
	Restart RestartState `json:"restart,omitempty"`
}
