package domain

import "time"

// EventType enumerates the initial lifecycle/command events (v1.md §12).
// The event bus that carries these must not be coupled to networking code,
// so a future scheduler can subscribe without depending on transport.
type EventType string

const (
	EventNodeDiscovered   EventType = "node.discovered"
	EventNodeRegistered   EventType = "node.registered"
	EventNodeConnected    EventType = "node.connected"
	EventNodeReady        EventType = "node.ready"
	EventNodeUpdated      EventType = "node.updated"
	EventNodeOffline      EventType = "node.offline"
	EventNodeReconnected  EventType = "node.reconnected"
	EventNodeRevoked      EventType = "node.revoked"
	EventNodeUnrevoked    EventType = "node.unrevoked"
	EventCommandSent      EventType = "command.sent"
	EventCommandCompleted EventType = "command.completed"
	EventCommandFailed    EventType = "command.failed"

	EventWorkloadAssigned  EventType = "workload.assigned"
	EventWorkloadStarted   EventType = "workload.started"
	EventWorkloadCompleted EventType = "workload.completed"
	EventWorkloadFailed    EventType = "workload.failed"
	EventWorkloadCanceled  EventType = "workload.canceled"
	EventWorkloadQueued    EventType = "workload.queued"
	EventWorkloadProgress  EventType = "workload.progress"
	EventJobSubmitted      EventType = "job.submitted"
	EventJobFinished       EventType = "job.finished"
	// An AI plan changed state (data: planId, state).
	EventPlanUpdated EventType = "plan.updated"
)

// A device asked to join by approval (agent -pair), and the operator's
// answer (approved / rejected / expired).
const (
	EventJoinRequested EventType = "node.join-requested"
	EventJoinDecided   EventType = "node.join-decided"
	// The join window opened or closed (data: open, remainingSeconds).
	EventJoinWindow EventType = "join.window"
)

// Event is a single state change emitted by the harness.
type Event struct {
	Type      EventType      `json:"type"`
	NodeID    NodeID         `json:"nodeId"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}
