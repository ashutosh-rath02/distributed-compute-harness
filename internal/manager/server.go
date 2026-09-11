package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/eventbus"
	"home-harness/internal/identity"
	"home-harness/internal/protocol"
)

// Config holds the manager's tunables.
type Config struct {
	Addr             string
	PairingToken     string
	HeartbeatTimeout time.Duration
	// ReconcileInterval is how often the reconciliation loop
	// (reconcile.go) checks for restart-eligible workloads. Deliberately
	// its own tunable, not derived from HeartbeatTimeout: heartbeat cadence
	// and restart-backoff resolution are different concerns. Defaults to
	// 5s (see cmd/manager's -reconcile-interval flag).
	ReconcileInterval time.Duration
	// AgentBinaryPath, if set, is the agent executable the manager serves
	// at /agent-binary (registered on the transport's own listener — see
	// cmd/manager/main.go) and hashes once at startup for self-update's
	// "does this node need updating" decision (selfupdate.go). Empty
	// disables self-update entirely rather than erroring — a manager
	// restarted without this flag simply can't push updates until it's
	// set again.
	AgentBinaryPath string
}

// PersistentStore is the subset of persistent storage the manager needs:
// last-known node manifests and workload records that must survive a
// restart. The manager depends only on this interface, never on the
// concrete store package (e.g. internal/store/persistent), so storage can
// change later without touching manager logic.
type PersistentStore interface {
	UpsertNode(manifest domain.Manifest) error
	ListNodes() ([]domain.Manifest, error)
	UpsertWorkload(pw domain.PersistedWorkload) error
	ListWorkloads() ([]domain.PersistedWorkload, error)
}

// Server is the control-plane process: it accepts connections over a
// domain.Transport, runs the REGISTER/HEARTBEAT protocol, and owns the
// node registry. Persistence is optional (nil store = registry only,
// nothing survives a restart) so tests and the walking-skeleton path from
// Milestone 3 keep working unchanged.
type Server struct {
	cfg       Config
	transport domain.Transport
	store     PersistentStore
	Registry  *Registry
	Workloads *WorkloadRegistry
	// Events publishes lifecycle/command events (v1.md §12) — a future
	// scheduler, the HTTP API's SSE stream, or a CLI can subscribe without
	// coupling to networking code.
	Events *eventbus.Bus

	pendingMu sync.Mutex
	pending   map[string]pendingCommand

	// agentBinaryHash is cfg.AgentBinaryPath's SHA-256, computed once at
	// startup (see NewServer) — empty if AgentBinaryPath is unset or
	// unreadable, in which case NeedsUpdate always reports false.
	agentBinaryHash string
}

// pendingCommand tracks who a dispatched command was sent to, so its
// COMMAND_RESULT can be rejected unless it actually comes back on that
// same node's connection — otherwise any registered node could forge a
// result for a command dispatched to a different node, by guessing or
// observing a command ID (the same spoofing class closed for HEARTBEAT).
type pendingCommand struct {
	target domain.NodeID
	result chan domain.CommandResult
}

// NewServer builds a manager bound to a concrete transport (e.g. the ws
// package) but coded only against domain.Transport, and an optional
// PersistentStore for node identity/metadata to survive a restart.
func NewServer(transport domain.Transport, store PersistentStore, cfg Config) *Server {
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = defaultReconcileInterval
	}
	s := &Server{
		cfg:       cfg,
		transport: transport,
		store:     store,
		Registry:  NewRegistry(),
		Workloads: NewWorkloadRegistry(),
		Events:    eventbus.New(),
		pending:   make(map[string]pendingCommand),
	}

	if cfg.AgentBinaryPath == "" {
		log.Println("manager: self-update disabled: -agent-binary not set")
	} else if hash, err := hashFile(cfg.AgentBinaryPath); err != nil {
		log.Printf("manager: self-update disabled: could not hash -agent-binary %q: %v", cfg.AgentBinaryPath, err)
	} else {
		s.agentBinaryHash = hash
	}

	return s
}

func (s *Server) publish(eventType domain.EventType, nodeID domain.NodeID, data map[string]any) {
	s.Events.Publish(domain.Event{Type: eventType, NodeID: nodeID, Timestamp: time.Now().UTC(), Data: data})
}

// ErrNodeNotConnected is returned by SendCommand when the target node has
// no live connection to send the command over.
var ErrNodeNotConnected = errors.New("manager: node is not connected")

// SendCommand dispatches a command to a specific node and blocks until a
// correlated COMMAND_RESULT arrives, ctx is canceled, or timeout elapses —
// this is what proves the manager can invoke an operation on a node and
// get a structured result back (v1.md §10).
func (s *Server) SendCommand(ctx context.Context, nodeID domain.NodeID, name domain.CommandName, args map[string]string, timeout time.Duration) (domain.CommandResult, error) {
	rec, ok := s.Registry.Get(nodeID)
	if !ok || rec.Conn == nil || rec.State != domain.NodeReady {
		return domain.CommandResult{}, ErrNodeNotConnected
	}

	cmdID, err := newRandomID()
	if err != nil {
		return domain.CommandResult{}, err
	}
	cmd := domain.Command{ID: cmdID, Name: name, Target: nodeID, Args: args}

	resultCh := make(chan domain.CommandResult, 1)
	s.pendingMu.Lock()
	s.pending[cmdID] = pendingCommand{target: nodeID, result: resultCh}
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, cmdID)
		s.pendingMu.Unlock()
	}()

	s.send(ctx, rec.Conn, protocol.MsgCommand, domain.ManagerNodeID, nodeID, protocol.CommandPayload{Command: cmd})
	log.Printf("command.sent: %s to %s (id=%s)", name, nodeID, cmdID)
	s.publish(domain.EventCommandSent, nodeID, map[string]any{"commandId": cmdID, "name": string(name)})

	sendCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	select {
	case result := <-resultCh:
		if result.Success {
			log.Printf("command.completed: %s to %s (id=%s)", name, nodeID, cmdID)
			s.publish(domain.EventCommandCompleted, nodeID, map[string]any{"commandId": cmdID, "name": string(name)})
		} else {
			log.Printf("command.failed: %s to %s (id=%s): %s", name, nodeID, cmdID, result.Error)
			s.publish(domain.EventCommandFailed, nodeID, map[string]any{"commandId": cmdID, "name": string(name), "error": result.Error})
		}
		return result, nil
	case <-sendCtx.Done():
		log.Printf("command.failed: %s to %s (id=%s): %v", name, nodeID, cmdID, sendCtx.Err())
		s.publish(domain.EventCommandFailed, nodeID, map[string]any{"commandId": cmdID, "name": string(name), "error": sendCtx.Err().Error()})
		return domain.CommandResult{}, sendCtx.Err()
	}
}

// failPendingCommandsFor resolves every in-flight command targeting
// nodeID with a failure, instead of leaving SendCommand's caller to block
// for the full timeout. Without this, a node going offline mid-command
// (churn is normal — baseline §9 rule 4) silently strands the pending
// entry: SendCommand still eventually returns via its own context/timeout
// deadline, but only after waiting needlessly for a result that can never
// arrive on a connection that's already gone.
func (s *Server) failPendingCommandsFor(nodeID domain.NodeID, reason string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, p := range s.pending {
		if p.target != nodeID {
			continue
		}
		delete(s.pending, id)
		select {
		case p.result <- domain.CommandResult{CommandID: id, Success: false, Error: reason}:
		default:
		}
	}
}

// ErrNoReadyNode is returned by SubmitWorkload when no target was given and
// no node is currently READY to receive one.
var ErrNoReadyNode = errors.New("manager: no target given and no node is currently ready")

// ErrNoEligibleNode is returned by SubmitWorkload when one or more nodes
// are READY but none satisfies the workload's stated resource
// requirements — distinct from ErrNoReadyNode, which means no node is
// READY at all.
var ErrNoEligibleNode = errors.New("manager: no node satisfies the workload's resource requirements")

// SubmitWorkload dispatches a workload for execution. If target is empty,
// placement is resolved via v2's resource-aware selectNode (v1.md §22),
// now also filtered by whether a node actually declares capability (v4).
// A non-Never restartPolicy makes this workload a "service" in v3's sense:
// the reconciliation loop (reconcile.go) will re-run it after it stops,
// per RestartPolicy.WantsRestartAfter.
func (s *Server) SubmitWorkload(ctx context.Context, target domain.NodeID, command string, args []string, capability domain.CapabilityName, params map[string]string, req domain.ResourceRequirements, restartPolicy domain.RestartPolicy) (domain.Workload, error) {
	if capability == "" {
		capability = domain.CapabilitySystemExecute
	}
	pinned := target != ""
	rec, target, err := s.resolveWorkloadTarget(target, capability, req)
	if err != nil {
		return domain.Workload{}, err
	}

	id, err := newRandomID()
	if err != nil {
		return domain.Workload{}, err
	}
	w := domain.Workload{ID: domain.WorkloadID(id), Target: target, Pinned: pinned, Command: command, Args: args, Capability: capability, Params: params, Requirements: req, RestartPolicy: restartPolicy}
	status := domain.WorkloadStatus{ID: w.ID, Target: target, State: domain.WorkloadPending}

	wrec := s.Workloads.Put(w, status)
	s.persistWorkloadRecord(wrec)

	s.send(ctx, rec.Conn, protocol.MsgWorkloadAssign, domain.ManagerNodeID, target, protocol.WorkloadAssignPayload{Workload: w})
	log.Printf("workload.assigned: %s to %s", w.ID, target)
	s.publish(domain.EventWorkloadAssigned, target, map[string]any{"workloadId": string(w.ID), "command": command, "capability": string(capability)})

	return w, nil
}

// resolveWorkloadTarget picks the node a workload will run on. An explicit
// target is still checked against req — specifying both a target and
// requirements is not a way to bypass them, since silently ignoring
// requirements for explicit targets would be a surprising inconsistency.
// An empty target selects among every READY, connected node via
// selectNode (v2's resource-aware placement, v1.md §22); with an empty req
// this keeps v1's eligible set unchanged, only its ordering becomes
// deterministic instead of arbitrary map order.
func (s *Server) resolveWorkloadTarget(target domain.NodeID, capability domain.CapabilityName, req domain.ResourceRequirements) (*NodeRecord, domain.NodeID, error) {
	if target != "" {
		rec, ok := s.Registry.Get(target)
		if !ok || rec.Conn == nil || rec.State != domain.NodeReady {
			return nil, "", ErrNodeNotConnected
		}
		if ok, reason := nodeFits(rec, capability, req); !ok {
			return nil, "", fmt.Errorf("%w (%s: %s)", ErrNoEligibleNode, target, reason)
		}
		return rec, target, nil
	}

	var candidates []*NodeRecord
	for _, rec := range s.Registry.List() {
		if rec.State == domain.NodeReady && rec.Conn != nil {
			candidates = append(candidates, rec)
		}
	}
	best, err := selectNode(candidates, capability, req)
	if err != nil {
		return nil, "", err
	}
	return best, best.Node.Identity.NodeID, nil
}

// CancelWorkload requests termination of a running workload. It only sends
// the request to the node — the workload's status transitions to CANCELED
// once the agent actually reports it via WORKLOAD_STATUS, not immediately.
func (s *Server) CancelWorkload(ctx context.Context, id domain.WorkloadID) error {
	wrec, ok := s.Workloads.Get(id)
	if !ok {
		return fmt.Errorf("manager: unknown workload %s", id)
	}
	rec, ok := s.Registry.Get(wrec.Workload.Target)
	if !ok || rec.Conn == nil {
		return ErrNodeNotConnected
	}
	s.send(ctx, rec.Conn, protocol.MsgWorkloadCancel, domain.ManagerNodeID, wrec.Workload.Target, protocol.WorkloadCancelPayload{ID: id})
	return nil
}

// persistWorkloadRecord persists a workload's full current record —
// request, status, and restart bookkeeping — as one unit, so a manager
// restart never sees a status without the restart count/backoff that goes
// with it (or vice versa).
func (s *Server) persistWorkloadRecord(rec WorkloadRecord) {
	if s.store == nil {
		return
	}
	pw := domain.PersistedWorkload{Workload: rec.Workload, Status: rec.Status, Restart: rec.Restart}
	if err := s.store.UpsertWorkload(pw); err != nil {
		log.Printf("manager: failed to persist workload %s: %v", rec.Workload.ID, err)
	}
}

// newRandomID generates a random 128-bit hex ID, used for both command and
// workload IDs.
// failWorkloadsFor marks every in-flight workload targeting nodeID as
// FAILED and persists the change, mirroring failPendingCommandsFor for
// workloads (which, unlike a Command, have a durable record to update, not
// just a caller to unblock).
//
// It also best-effort sends WORKLOAD_CANCEL over the node's registry
// connection, if any. This matters specifically for the heartbeat-timeout
// path (monitorHeartbeats -> ExpireStale): a node that's merely slow to
// heartbeat, not actually disconnected, still has a live rec.Conn here,
// and the process this function is about to mark FAILED (and which v3's
// reconciler may restart elsewhere) could still genuinely be running. This
// is not airtight — the CANCEL itself can be lost on a degraded one-way
// link — but it meaningfully reduces the chance of two live instances of
// the same restart-policy workload existing at once, using a message the
// agent already knows how to handle. On the connection-actually-closed
// path (handleConn's defer), this send is a harmless no-op.
func (s *Server) failWorkloadsFor(ctx context.Context, nodeID domain.NodeID, reason string) {
	var conn domain.Conn
	if rec, ok := s.Registry.Get(nodeID); ok {
		conn = rec.Conn
	}

	for _, rec := range s.Workloads.FailInFlightFor(nodeID, reason) {
		s.persistWorkloadRecord(rec)
		log.Printf("workload.failed: %s (%s)", rec.Workload.ID, reason)
		s.publish(domain.EventWorkloadFailed, nodeID, map[string]any{"workloadId": string(rec.Workload.ID), "error": reason})
		if conn != nil {
			s.send(ctx, conn, protocol.MsgWorkloadCancel, domain.ManagerNodeID, nodeID, protocol.WorkloadCancelPayload{ID: rec.Workload.ID})
		}
	}
}

func newRandomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("manager: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Run loads any previously-known nodes from the persistent store, then
// starts accepting connections and monitoring heartbeats until ctx is
// canceled.
func (s *Server) Run(ctx context.Context) error {
	if s.store != nil {
		manifests, err := s.store.ListNodes()
		if err != nil {
			return fmt.Errorf("manager: load persisted nodes: %w", err)
		}
		for _, m := range manifests {
			s.Registry.Seed(m)
		}

		workloads, err := s.store.ListWorkloads()
		if err != nil {
			return fmt.Errorf("manager: load persisted workloads: %w", err)
		}
		for _, pw := range workloads {
			s.Workloads.Seed(pw)
		}
	}

	conns, err := s.transport.Listen(ctx, s.cfg.Addr)
	if err != nil {
		return err
	}

	go s.monitorHeartbeats(ctx)
	go s.reconcileWorkloads(ctx)

	for {
		select {
		case conn, ok := <-conns:
			if !ok {
				return nil
			}
			go s.handleConn(ctx, conn)
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *Server) monitorHeartbeats(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.HeartbeatTimeout / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, id := range s.Registry.ExpireStale(s.cfg.HeartbeatTimeout) {
				log.Printf("node.offline: %s", id)
				s.publish(domain.EventNodeOffline, id, map[string]any{"reason": "heartbeat timeout"})
				s.failPendingCommandsFor(id, "node went offline: heartbeat timeout")
				s.failWorkloadsFor(ctx, id, "node went offline: heartbeat timeout")
			}
		}
	}
}

func (s *Server) handleConn(ctx context.Context, conn domain.Conn) {
	var nodeID domain.NodeID
	defer func() {
		if nodeID != "" && s.Registry.SetOfflineIfCurrent(nodeID, conn) {
			log.Printf("node.offline: %s (connection closed)", nodeID)
			s.publish(domain.EventNodeOffline, nodeID, map[string]any{"reason": "connection closed"})
			s.failPendingCommandsFor(nodeID, "node went offline: connection closed")
			s.failWorkloadsFor(ctx, nodeID, "node went offline: connection closed")
		}
		conn.Close()
	}()

	for {
		data, err := conn.Receive(ctx)
		if err != nil {
			return
		}
		env, err := protocol.Decode(data)
		if err != nil {
			log.Printf("manager: decode error from %s: %v", conn.RemoteAddr(), err)
			continue
		}
		if err := protocol.CheckVersion(env); err != nil {
			s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "UNSUPPORTED_VERSION", err.Error())
			continue
		}

		// Every message type except REGISTER requires this connection to
		// already be bound to a verified identity. Trusting env.Source
		// instead would let a registered connection forge messages on
		// behalf of a different node (baseline §5: network presence must
		// never imply execution authority — that extends to claimed
		// identity on an already-open connection, not just to admission).
		if env.Type != protocol.MsgRegister && nodeID == "" {
			s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "NOT_REGISTERED", "register before sending other messages")
			continue
		}

		switch env.Type {
		case protocol.MsgRegister:
			nodeID = s.handleRegister(ctx, conn, env)
		case protocol.MsgHeartbeat:
			s.handleHeartbeat(nodeID, env)
		case protocol.MsgPing:
			s.handlePing(ctx, conn, nodeID)
		case protocol.MsgCommandResult:
			s.handleCommandResult(nodeID, env)
		case protocol.MsgCapabilityUpdate:
			s.handleCapabilityUpdate(nodeID, env)
		case protocol.MsgWorkloadStatus:
			s.handleWorkloadStatus(nodeID, env)
		default:
			s.sendError(ctx, conn, domain.ManagerNodeID, nodeID, "UNSUPPORTED_TYPE", string(env.Type))
		}
	}
}

func (s *Server) handleRegister(ctx context.Context, conn domain.Conn, env *protocol.Envelope) domain.NodeID {
	var payload protocol.RegisterPayload
	if err := env.DecodePayload(&payload); err != nil {
		s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "BAD_PAYLOAD", err.Error())
		return ""
	}

	node := payload.Manifest.Node
	claimedID := node.Identity.NodeID

	// Recompute the NodeID from the claimed public key rather than trusting
	// the claimed NodeID field as given — otherwise NodeID is just a
	// string with no cryptographic meaning.
	if identity.DeriveNodeID(node.Identity.PublicKey) != claimedID {
		s.reject(ctx, conn, env.Source, "identity mismatch: NodeID does not match public key")
		return ""
	}

	// Verify proof of possession of the private key for that public key.
	// Without this, knowing a valid pairing token would be enough to
	// register as ANY existing NodeID.
	signed := protocol.RegisterSignedData(payload.PairingToken, claimedID)
	if !identity.Verify(node.Identity, signed, payload.Signature) {
		s.reject(ctx, conn, env.Source, "invalid signature: proof of key possession failed")
		return ""
	}

	if payload.PairingToken != s.cfg.PairingToken {
		s.reject(ctx, conn, env.Source, "invalid pairing token")
		return ""
	}

	_, isNew := s.Registry.Upsert(payload.Manifest, conn)
	s.Registry.SetState(claimedID, domain.NodeReady)

	if s.store != nil {
		if err := s.store.UpsertNode(payload.Manifest); err != nil {
			log.Printf("manager: failed to persist node %s: %v", claimedID, err)
		}
	}

	if isNew {
		log.Printf("node.registered: %s (%s)", claimedID, node.Name)
		s.publish(domain.EventNodeRegistered, claimedID, map[string]any{"name": node.Name})
	} else {
		log.Printf("node.reconnected: %s (%s)", claimedID, node.Name)
		s.publish(domain.EventNodeReconnected, claimedID, map[string]any{"name": node.Name})
	}
	s.publish(domain.EventNodeReady, claimedID, nil)

	s.send(ctx, conn, protocol.MsgRegisterAck, domain.ManagerNodeID, claimedID,
		protocol.RegisterAckPayload{NodeID: claimedID, ServerTime: time.Now().UTC()})

	return claimedID
}

func (s *Server) reject(ctx context.Context, conn domain.Conn, dest domain.NodeID, reason string) {
	s.send(ctx, conn, protocol.MsgRegisterReject, domain.ManagerNodeID, dest,
		protocol.RegisterRejectPayload{Reason: reason})
	log.Printf("manager: rejected registration from %s: %s", conn.RemoteAddr(), reason)
}

func (s *Server) handleHeartbeat(nodeID domain.NodeID, env *protocol.Envelope) {
	var payload protocol.HeartbeatPayload
	if err := env.DecodePayload(&payload); err != nil {
		log.Printf("manager: bad heartbeat payload from %s: %v", nodeID, err)
		return
	}
	s.Registry.RecordHeartbeat(nodeID, payload.RuntimeState)
}

func (s *Server) handlePing(ctx context.Context, conn domain.Conn, nodeID domain.NodeID) {
	s.send(ctx, conn, protocol.MsgPong, domain.ManagerNodeID, nodeID, nil)
}

func (s *Server) handleCommandResult(nodeID domain.NodeID, env *protocol.Envelope) {
	var payload protocol.CommandResultPayload
	if err := env.DecodePayload(&payload); err != nil {
		log.Printf("manager: bad command result payload from %s: %v", nodeID, err)
		return
	}

	s.pendingMu.Lock()
	pending, ok := s.pending[payload.Result.CommandID]
	if ok {
		delete(s.pending, payload.Result.CommandID)
	}
	s.pendingMu.Unlock()

	if !ok {
		log.Printf("manager: command result from %s for unknown/expired command %s", nodeID, payload.Result.CommandID)
		return
	}
	if pending.target != nodeID {
		log.Printf("manager: ignoring command result from %s claiming to answer a command sent to %s", nodeID, pending.target)
		return
	}

	pending.result <- payload.Result
}

func (s *Server) handleCapabilityUpdate(nodeID domain.NodeID, env *protocol.Envelope) {
	var payload protocol.CapabilityUpdatePayload
	if err := env.DecodePayload(&payload); err != nil {
		log.Printf("manager: bad capability update payload from %s: %v", nodeID, err)
		return
	}
	s.Registry.UpdateResources(nodeID, payload.Resources, payload.Capabilities)
	log.Printf("node.updated: %s (resources/capabilities refreshed)", nodeID)
	s.publish(domain.EventNodeUpdated, nodeID, nil)
}

// handleWorkloadStatus records a workload status update from a node. Like
// handleCommandResult, it rejects a report unless it actually comes from
// that workload's target node's connection — otherwise any registered node
// could forge status for a workload assigned to a different node.
//
// This same check also (harmlessly) fires for a more benign reason once
// restarts exist (reconcile.go): a superseded attempt's tardy terminal
// status can arrive from its old node after the reconciler has already
// re-placed the workload on a new one. That's correctly dropped here too —
// not every rejection on this path is an identity-forgery attempt.
func (s *Server) handleWorkloadStatus(nodeID domain.NodeID, env *protocol.Envelope) {
	var payload protocol.WorkloadStatusPayload
	if err := env.DecodePayload(&payload); err != nil {
		log.Printf("manager: bad workload status payload from %s: %v", nodeID, err)
		return
	}

	// Checked against a Get snapshot before mutating: UpdateStatus writes
	// unconditionally, so validating the target only *after* calling it
	// would let a rejected report still corrupt the live record first —
	// harmless for a one-shot workload, but dangerous once restarts exist
	// (a stale FAILED from a superseded node could make a genuinely
	// healthy restart attempt look crashed, and it also self-corrupts
	// domain.Workload.Target, which restarts and persistence rely on).
	current, ok := s.Workloads.Get(payload.Status.ID)
	if !ok {
		log.Printf("manager: workload status from %s for unknown workload %s", nodeID, payload.Status.ID)
		return
	}
	if current.Workload.Target != nodeID {
		log.Printf("manager: ignoring workload status from %s claiming to report on a workload assigned to %s (may be a stale report from a superseded restart attempt, not necessarily a forgery)", nodeID, current.Workload.Target)
		return
	}

	wrec, _ := s.Workloads.UpdateStatus(payload.Status)
	s.persistWorkloadRecord(wrec)

	switch payload.Status.State {
	case domain.WorkloadRunning:
		log.Printf("workload.started: %s on %s", payload.Status.ID, nodeID)
		s.publish(domain.EventWorkloadStarted, nodeID, map[string]any{"workloadId": string(payload.Status.ID)})
	case domain.WorkloadCompleted:
		log.Printf("workload.completed: %s on %s", payload.Status.ID, nodeID)
		s.publish(domain.EventWorkloadCompleted, nodeID, map[string]any{"workloadId": string(payload.Status.ID), "exitCode": payload.Status.ExitCode})
	case domain.WorkloadFailed:
		log.Printf("workload.failed: %s on %s: %s", payload.Status.ID, nodeID, payload.Status.Error)
		s.publish(domain.EventWorkloadFailed, nodeID, map[string]any{"workloadId": string(payload.Status.ID), "error": payload.Status.Error})
	case domain.WorkloadCanceled:
		log.Printf("workload.canceled: %s on %s", payload.Status.ID, nodeID)
		s.publish(domain.EventWorkloadCanceled, nodeID, map[string]any{"workloadId": string(payload.Status.ID)})
	}
}

func (s *Server) sendError(ctx context.Context, conn domain.Conn, source, dest domain.NodeID, code, message string) {
	s.send(ctx, conn, protocol.MsgError, source, dest, protocol.ErrorPayload{Code: code, Message: message})
}

func (s *Server) send(ctx context.Context, conn domain.Conn, msgType protocol.MessageType, source, dest domain.NodeID, payload any) {
	env, err := protocol.NewEnvelope(msgType, source, dest, payload)
	if err != nil {
		log.Printf("manager: build envelope: %v", err)
		return
	}
	wire, err := protocol.Encode(env)
	if err != nil {
		log.Printf("manager: encode envelope: %v", err)
		return
	}
	if err := conn.Send(ctx, wire); err != nil {
		log.Printf("manager: send to %s: %v", conn.RemoteAddr(), err)
	}
}
