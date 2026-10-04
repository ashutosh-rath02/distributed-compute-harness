package agent

import (
	"context"
	"errors"
	"log"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

// handleWorkloadAssign starts executing an incoming WORKLOAD_ASSIGN via the
// agent's Executor. The running/terminal status updates it reports are
// pushed back to the manager as WORKLOAD_STATUS messages as they happen,
// not just at the end — the manager sees RUNNING as soon as the subprocess
// actually starts.
func (a *Agent) handleWorkloadAssign(ctx context.Context, conn domain.Conn, env *protocol.Envelope) {
	var payload protocol.WorkloadAssignPayload
	if err := env.DecodePayload(&payload); err != nil {
		return // malformed assignment from the manager: nothing sensible to run
	}
	wl := payload.Workload
	log.Printf("agent %s: WORKLOAD_ASSIGN %s: %s %v", a.identity.NodeID, wl.ID, wl.Command, wl.Args)

	if a.cfg.InsecureWorkloadsDisabled {
		a.sendWorkloadStatus(ctx, conn, domain.WorkloadStatus{
			ID: wl.ID, Target: wl.Target, State: domain.WorkloadFailed,
			Error: "workload execution disabled: agent is running with -insecure and cannot verify the manager's identity",
		})
		return
	}

	var xfer ArtifactTransfer
	if wl.HasFiles() {
		var terr error
		if xfer, terr = a.artifactTransfer(wl.ID, payload.ArtifactToken); terr != nil {
			a.sendWorkloadStatus(ctx, conn, domain.WorkloadStatus{
				ID: wl.ID, Target: wl.Target, State: domain.WorkloadFailed, Error: "workload files: " + terr.Error(),
			})
			return
		}
	}
	err := a.executor.StartWithFiles(ctx, wl, xfer, func(status domain.WorkloadStatus) {
		a.sendWorkloadStatus(ctx, conn, status)
		if status.State != domain.WorkloadRunning && changesModels(wl.EffectiveCapability()) {
			a.requestReprobe() // the manager learns the new model list now, not in 30 s
		}
	})
	if err != nil {
		// Rejected before it ever ran — report FAILED rather than leaving
		// the manager waiting for a status that will never come. A full
		// executor (a race with the manager's view of free slots) is
		// marked retryable so the manager re-queues it instead.
		a.sendWorkloadStatus(ctx, conn, domain.WorkloadStatus{
			ID: wl.ID, Target: wl.Target, State: domain.WorkloadFailed, Error: err.Error(),
			Retryable: errors.Is(err, ErrExecutorFull),
		})
	}
}

// handleWorkloadCancel requests termination of a running workload. A
// mismatch (already finished, wrong ID) is logged, not reported back to
// the manager as an event — the manager will already have (or shortly
// receive) that workload's terminal WORKLOAD_STATUS either way.
func (a *Agent) handleWorkloadCancel(ctx context.Context, conn domain.Conn, env *protocol.Envelope) {
	var payload protocol.WorkloadCancelPayload
	if err := env.DecodePayload(&payload); err != nil {
		return
	}
	log.Printf("agent %s: WORKLOAD_CANCEL %s", a.identity.NodeID, payload.ID)
	if err := a.executor.Cancel(payload.ID); err != nil {
		log.Printf("agent %s: WORKLOAD_CANCEL %s: %v", a.identity.NodeID, payload.ID, err)
	}
}

func (a *Agent) sendWorkloadStatus(ctx context.Context, conn domain.Conn, status domain.WorkloadStatus) {
	log.Printf("agent %s: workload %s -> %s", a.identity.NodeID, status.ID, status.State)
	if err := a.send(ctx, conn, protocol.MsgWorkloadStatus, domain.ManagerNodeID, protocol.WorkloadStatusPayload{Status: status}); err != nil {
		log.Printf("agent %s: send WORKLOAD_STATUS for %s: %v", a.identity.NodeID, status.ID, err)
	}
}
