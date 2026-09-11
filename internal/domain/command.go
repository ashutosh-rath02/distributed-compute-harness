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
	// CommandSelfUpdate tells the agent to download, verify, and swap in a
	// new binary of itself, then relaunch with its original flags — see
	// internal/agent/selfupdate.go. Args carries "sha256", the expected
	// hex digest of the binary the manager serves at /agent-binary; unlike
	// every other command, an agent that handles this one successfully
	// never gets the chance to report failure/success the normal way
	// (see handleCommand's special-casing of this name) since the process
	// that would report it is the one being replaced.
	CommandSelfUpdate CommandName = "SELF_UPDATE"
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
