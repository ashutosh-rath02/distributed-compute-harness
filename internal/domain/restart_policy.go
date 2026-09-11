package domain

import (
	"fmt"
	"time"
)

// RestartPolicy declares whether a workload should be re-run after it
// stops. This is v3's entire "service" concept (v1.md §22) — a service is
// just a workload with a restart policy, not a separate domain type
// (Home_Compute_Harness_Baseline_Architecture.md §3: "Workload... Job,
// service, capability invocation"). It is a static, declarative field, not
// a policy engine (v1.md §19 excludes a "complex policy engine").
type RestartPolicy string

const (
	// RestartNever is the zero value: a workload never restarts once it
	// reaches a terminal state, exactly matching pre-v3 behavior. Kept as
	// "" (not "never") so it's omitted from JSON entirely and every
	// existing caller/persisted record is byte-identical to before v3.
	RestartNever RestartPolicy = ""
	// RestartOnFailure restarts only on a non-zero exit (FAILED) or an
	// unresolved outcome after a manager restart (UNKNOWN) — a clean exit
	// (COMPLETED) is left alone, matching the common "keep retrying until
	// it succeeds" use case without also re-running work that finished.
	RestartOnFailure RestartPolicy = "on-failure"
	// RestartAlways restarts on any terminal state except an explicit
	// CANCELED — including a clean exit — for a workload meant to simply
	// always be running (e.g. a long-lived server process).
	RestartAlways RestartPolicy = "always"
)

// ParseRestartPolicy parses the API/CLI string form. "" and "never" both
// map to RestartNever so an omitted field and an explicit "never" behave
// identically.
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	switch s {
	case "", "never":
		return RestartNever, nil
	case string(RestartOnFailure):
		return RestartOnFailure, nil
	case string(RestartAlways):
		return RestartAlways, nil
	default:
		return "", fmt.Errorf("invalid restart policy %q: want \"never\", \"on-failure\", or \"always\"", s)
	}
}

// WantsRestartAfter reports whether a workload with this policy should be
// re-run given it just reached state. Centralized here, next to the
// vocabulary it interprets, so CANCELED's terminality — v3's answer to
// "stop a service": reuse the existing CancelWorkload/WORKLOAD_CANCEL
// mechanism, no new "stop" command — is stated once, not re-derived at
// every call site. PENDING/RUNNING are never restart-eligible (already in
// flight) regardless of policy.
func (p RestartPolicy) WantsRestartAfter(state WorkloadState) bool {
	switch state {
	case WorkloadCompleted:
		return p == RestartAlways
	case WorkloadFailed, WorkloadUnknown:
		return p == RestartOnFailure || p == RestartAlways
	default:
		return false
	}
}

// RestartState is the manager's own bookkeeping for one workload's restart
// history. It is never sent to or received from an agent — unlike
// WorkloadStatus, which the agent constructs from scratch on every report
// and would silently zero this out if it lived there instead.
type RestartState struct {
	// Count is the lifetime total number of restarts, for display — it
	// never resets, the same way Kubernetes' own container restartCount
	// doesn't.
	Count int `json:"count,omitempty"`
	// BackoffCount drives the backoff curve specifically and is distinct
	// from Count: it resets to 0 whenever the previous attempt stayed
	// running long enough to count as healthy (see MarkRestarting), so a
	// service that ran fine for hours before one crash gets a short
	// backoff, not the multi-minute wait its full lifetime Count would
	// otherwise imply.
	BackoffCount int `json:"backoffCount,omitempty"`
	// NextRestartAt is when the reconciliation loop should next consider
	// this workload for a restart attempt — set via a backoff curve for a
	// genuine failure, or a short fixed delay for a deferred (not yet
	// possible) attempt, so a crash-looping workload isn't hammered at
	// full tick frequency.
	NextRestartAt time.Time `json:"nextRestartAt,omitempty"`
}
