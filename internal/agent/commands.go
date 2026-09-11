package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
	"home-harness/internal/sysinfo"
)

// handleCommand decodes and executes an incoming COMMAND, then sends a
// correlated COMMAND_RESULT back — this is the request/response loop that
// proves the harness can invoke an operation on a node (v1.md §10).
func (a *Agent) handleCommand(ctx context.Context, conn domain.Conn, env *protocol.Envelope, errCh chan<- error) {
	var payload protocol.CommandPayload
	if err := env.DecodePayload(&payload); err != nil {
		return // malformed command from the manager: nothing sensible to reply with
	}

	// SELF_UPDATE is special-cased ahead of the generic executeCommand
	// dispatch below: unlike every other command, the goroutine performing
	// it (performSelfUpdate) ends this process, so "go f(); return ack"
	// would not actually guarantee the ack goes out first — go doesn't
	// yield, and the spawned goroutine can start running immediately on
	// another core. Sending the ack synchronously here first, then
	// launching the goroutine, makes the ordering explicit rather than
	// timing-dependent.
	if payload.Command.Name == domain.CommandSelfUpdate {
		result := domain.CommandResult{CommandID: payload.Command.ID, Success: true, Output: map[string]string{"status": "update started"}}
		if err := a.send(ctx, conn, protocol.MsgCommandResult, env.Source, protocol.CommandResultPayload{Result: result}); err != nil {
			errCh <- fmt.Errorf("send COMMAND_RESULT: %w", err)
			return
		}
		go a.performSelfUpdate(payload.Command.Args["sha256"])
		return
	}

	result := a.executeCommand(ctx, conn, payload.Command)
	if err := a.send(ctx, conn, protocol.MsgCommandResult, env.Source, protocol.CommandResultPayload{Result: result}); err != nil {
		errCh <- fmt.Errorf("send COMMAND_RESULT: %w", err)
	}
}

// executeCommand runs one of the safe, harmless v0 commands (v1.md §10/§19
// — no remote shell). Command names outside this set fail with an error
// result rather than being silently ignored, so a caller always gets a
// structured answer.
func (a *Agent) executeCommand(ctx context.Context, conn domain.Conn, cmd domain.Command) domain.CommandResult {
	switch cmd.Name {
	case domain.CommandPing:
		return domain.CommandResult{CommandID: cmd.ID, Success: true, Output: map[string]string{"message": "pong"}}

	case domain.CommandEcho:
		return domain.CommandResult{CommandID: cmd.ID, Success: true, Output: map[string]string{"message": cmd.Args["message"]}}

	case domain.CommandGetSystemInfo:
		return a.getSystemInfo(ctx, cmd.ID)

	case domain.CommandGetAgentStatus:
		return domain.CommandResult{
			CommandID: cmd.ID,
			Success:   true,
			Output: map[string]string{
				"nodeId":       string(a.identity.NodeID),
				"state":        string(domain.NodeReady),
				"agentVersion": a.cfg.AgentVersion,
				"uptime":       time.Since(a.startedAt).String(),
			},
		}

	case domain.CommandRequestResourceRefresh:
		return a.refreshResources(ctx, conn, cmd.ID)

	default:
		return domain.CommandResult{CommandID: cmd.ID, Success: false, Error: fmt.Sprintf("unsupported command: %s", cmd.Name)}
	}
}

func (a *Agent) getSystemInfo(ctx context.Context, cmdID string) domain.CommandResult {
	hostname, _ := os.Hostname()
	resources, _, err := sysinfo.Manifest(ctx)
	if err != nil {
		return domain.CommandResult{CommandID: cmdID, Success: false, Error: err.Error()}
	}

	output := map[string]string{
		"hostname":     hostname,
		"os":           runtime.GOOS,
		"architecture": runtime.GOARCH,
		"agentVersion": a.cfg.AgentVersion,
	}
	for _, r := range resources {
		output[string(r.Kind)] = strconv.FormatFloat(r.Capacity, 'f', -1, 64) + " " + r.Unit
	}

	return domain.CommandResult{CommandID: cmdID, Success: true, Output: output}
}

// refreshResources re-collects this node's resources/capabilities and
// pushes them to the manager via CAPABILITY_UPDATE, in addition to
// returning a COMMAND_RESULT — the caller learns the command succeeded,
// and the manager's registry gets the refreshed values via the pushed
// update regardless of whether anyone is still waiting on the result.
func (a *Agent) refreshResources(ctx context.Context, conn domain.Conn, cmdID string) domain.CommandResult {
	resources, capabilities, err := sysinfo.Manifest(ctx)
	if err != nil {
		return domain.CommandResult{CommandID: cmdID, Success: false, Error: err.Error()}
	}

	update := protocol.CapabilityUpdatePayload{Resources: resources, Capabilities: capabilities}
	if err := a.send(ctx, conn, protocol.MsgCapabilityUpdate, domain.ManagerNodeID, update); err != nil {
		log.Printf("agent %s: send CAPABILITY_UPDATE: %v", a.identity.NodeID, err)
	}

	return domain.CommandResult{
		CommandID: cmdID,
		Success:   true,
		Output:    map[string]string{"resourceCount": strconv.Itoa(len(resources))},
	}
}
