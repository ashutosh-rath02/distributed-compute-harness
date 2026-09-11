package domain

// CommandName enumerates the safe, harmless commands v0 actually
// implements (v1.md §10). Additional capability names may be advertised in
// a manifest without a corresponding CommandName here — that is how future
// versions add real execution without redesigning this type.
type CommandName string

const (
	CommandPing                   CommandName = "PING"
	CommandEcho                   CommandName = "ECHO"
	CommandGetSystemInfo          CommandName = "GET_SYSTEM_INFO"
	CommandGetAgentStatus         CommandName = "GET_AGENT_STATUS"
	CommandRequestResourceRefresh CommandName = "REQUEST_RESOURCE_REFRESH"
)

// Command is a request dispatched from the manager to a specific node.
type Command struct {
	ID     string            `json:"id"`
	Name   CommandName       `json:"name"`
	Target NodeID            `json:"target"`
	Args   map[string]string `json:"args,omitempty"`
}

// CommandResult is a node's structured reply to a Command, correlated by
// CommandID.
type CommandResult struct {
	CommandID string            `json:"commandId"`
	Success   bool              `json:"success"`
	Output    map[string]string `json:"output,omitempty"`
	Error     string            `json:"error,omitempty"`
}
