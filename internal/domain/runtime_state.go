package domain

import "time"

// RuntimeState is the fast-changing half of a node's state (see v1.md §13
// "Runtime state" vs "Persistent state"). It must never be persisted
// alongside identity/config — a future distributed control plane needs
// this seam intact.
type RuntimeState struct {
	NodeID               NodeID        `json:"nodeId"`
	State                NodeState     `json:"state"`
	CPUPercent           float64       `json:"cpuPercent"`
	MemoryAvailableBytes uint64        `json:"memoryAvailableBytes"`
	Uptime               time.Duration `json:"uptime"`
	LastHeartbeat        time.Time     `json:"lastHeartbeat"`
	ConnectedAt          time.Time     `json:"connectedAt"`
	AgentVersion         string        `json:"agentVersion"`
}
