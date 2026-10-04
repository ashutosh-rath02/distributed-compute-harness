package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/eventbus"
	"home-harness/internal/identity"
	"home-harness/internal/keepawake"
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
	// AgentBinaries is the agent-build catalog (agentcatalog.go): at most
	// one executable per platform, served at /agent-binaries/{os}/{arch}
	// on the agent transports and hashed once at startup for onboarding
	// and self-update. Empty disables both rather than erroring — a
	// manager started without -agent-binary simply can't hand out or push
	// agent binaries until restarted with one.
	AgentBinaries []AgentBinary
	// Fingerprint is the manager's own TLS fingerprint (mtls.Fingerprint),
	// the same value cmd/manager/main.go already logs on startup for an
	// operator to copy into -manager-fingerprint by hand. Stored here too so
	// GET /join-info (api.go) can hand it out over the manager's own
	// loopback API instead — see cmd/harnessctl's `join` command. Empty
	// when running -insecure.
	Fingerprint string
	// RelayAddr/RelayToken are surfaced only through the loopback operator
	// API so onboarding scripts can configure an off-LAN agent without the
	// operator retyping the manager's relay settings.
	RelayAddr           string
	RelayToken          string
	RelayPublicURL      string
	EnrollmentTTL       time.Duration
	EnrollmentPublisher EnrollmentPublisher
	// OperatorToken is the operator API credential (operatorauth.go).
	// Empty disables operator authentication — only for tests and
	// embedding; cmd/manager always loads or creates one.
	OperatorToken string
	// AIKey is the OpenAI-compatible API's own credential (openai.go):
	// accepted only on /v1/. Empty: only the operator credentials work.
	AIKey string
	// Artifacts stores workload input/output files (artifacts.go); nil
	// disables workload files. ArtifactRetention is how long an unused
	// artifact is kept (default 7 days).
	Artifacts         *artifacts.Store
	ArtifactRetention time.Duration
	// AdvertiseAddr is this machine's LAN address for agents (host:port),
	// offered pre-filled where the dashboard asks for it. Optional.
	AdvertiseAddr string
	// JoinAddr is where the join page listens (-join-addr, plain HTTP;
	// empty = no join page), and AppAPK the Android app's own APK it offers
	// for download (joinpage.go). Both optional.
	JoinAddr string
	AppAPK   string
	// FirstRunJoinWindow is how long adding devices (joinwindow.go) stays
	// open when the manager starts knowing no devices at all; 0 = closed
	// until the operator opens it.
	FirstRunJoinWindow time.Duration
	// InitialPolicy is the policy used until one is stored (policy.go):
	// cmd/manager passes domain.DefaultPolicy (raw commands off). Nil =
	// domain.PermissivePolicy, for embedding and tests.
	InitialPolicy *domain.Policy
	// KeepAwake asks the OS not to sleep while workloads run or are
	// being handed out (and for a couple of minutes after), since a
	// sleeping manager stops the whole fleet. Never blocks a sleep the
	// user asks for.
	KeepAwake bool
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
	// DeleteWorkloads forgets finished workloads past retention (retention.go).
	DeleteWorkloads(ids []domain.WorkloadID) error
	// RevokeNode must persist the denylist entry and forget the node's
	// record atomically (see revocation.go).
	RevokeNode(rev domain.RevokedNode) error
	ListRevoked() ([]domain.RevokedNode, error)
	UnrevokeNode(id domain.NodeID) error
	// Operator metadata and the audit log (fleet.go, audit.go).
	PutNodeMeta(id domain.NodeID, meta domain.NodeMeta) error
	ListNodeMeta() (map[domain.NodeID]domain.NodeMeta, error)
	AppendAudit(e domain.AuditEntry, keep int) (domain.AuditEntry, error)
	ListAudit(log domain.AuditLog, limit int) ([]domain.AuditEntry, error)
	// Batch jobs (jobs.go).
	UpsertJob(job domain.Job) error
	ListJobs() ([]domain.Job, error)
	// Workflows (workflows.go).
	UpsertWorkflow(wf domain.Workflow) error
	ListWorkflows() ([]domain.Workflow, error)
	// Policy (policy.go).
	GetPolicy() (domain.Policy, bool, error)
	PutPolicy(p domain.Policy) error
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
	Events      *eventbus.Bus
	enrollments *enrollmentStore
	joinReqs    *joinRequests
	joinWin     *joinWindow

	// admitMu serializes admission (handleRegister's revocation check
	// through its registry Upsert) against RevokeNode/UnrevokeNode, so a
	// REGISTER that passed the check can never re-add a node a concurrent
	// revocation has just removed.
	admitMu     sync.Mutex
	revocations *revocationList

	meta *nodeMetaStore
	// tunnels: device-to-device tunnel grants and waiting connections.
	tunnels *tunnelTable
	// splits: the split session, if any (split.go).
	splits *splitTable
	// awake is held while work is in flight (keepAwakeTick); nil unless
	// Config.KeepAwake. awakeUntil is touched only by the reconcile loop.
	awake      *keepawake.Request
	awakeUntil time.Time
	// metaMu serializes read-modify-write changes to node metadata (the
	// alias/labels and the availability rule are set separately).
	metaMu   sync.Mutex
	auditLog *auditRecorder

	// placeMu serializes "check room, then reserve" (placement plus the
	// state change that makes the reservation count), so two concurrent
	// submissions can never both take a node's last slot.
	placeMu      sync.Mutex
	dispatchKick chan struct{}
	requeueMu    sync.Mutex
	requeues     map[domain.WorkloadID]int

	pendingMu sync.Mutex
	pending   map[string]pendingCommand

	// grants authorizes agents' workload file transfers (artifacts.go).
	grants *grantTable
	// jobs holds batch jobs (jobs.go).
	jobs *jobTable
	// workflows holds multi-step workflows (workflows.go).
	workflows *workflowTable
	// spots holds spot checks and suspect marks (spotcheck.go).
	spots *spotCheckTable
	// plans holds AI plans (planner.go), in memory only.
	plans *planTable
	// policy decides what may run (policy.go).
	policy *policyStore

	// agents is the loaded agent-build catalog (agentcatalog.go), built
	// once at startup from cfg.AgentBinaries — never nil.
	agents *agentCatalog
	// app: the Android app offered to agents updated with it (nil = none).
	app *agentBuild
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
	if cfg.EnrollmentTTL <= 0 {
		cfg.EnrollmentTTL = defaultEnrollmentTTL
	}
	s := &Server{
		cfg:          cfg,
		transport:    transport,
		store:        store,
		Registry:     NewRegistry(),
		Workloads:    NewWorkloadRegistry(),
		Events:       eventbus.New(),
		pending:      make(map[string]pendingCommand),
		enrollments:  newEnrollmentStore(),
		joinReqs:     newJoinRequests(),
		joinWin:      newJoinWindow(),
		revocations:  newRevocationList(),
		meta:         newNodeMetaStore(),
		auditLog:     newAuditRecorder(),
		dispatchKick: make(chan struct{}, 1),
		requeues:     make(map[domain.WorkloadID]int),
		grants:       newGrantTable(),
		tunnels:      newTunnelTable(),
		splits:       newSplitTable(),
		jobs:         newJobTable(),
		workflows:    newWorkflowTable(),
		spots:        newSpotCheckTable(),
		plans:        newPlanTable(),
		policy:       &policyStore{p: domain.PermissivePolicy()},
	}
	if cfg.KeepAwake {
		s.awake = keepawake.New("Home Harness manager: your devices are working on tasks")
	}
	if cfg.InitialPolicy != nil {
		s.policy.set(*cfg.InitialPolicy)
	}

	if cfg.OperatorToken == "" {
		log.Println("manager: operator API authentication disabled (no operator token configured)")
	}
	s.agents = &agentCatalog{}
	if len(cfg.AgentBinaries) == 0 {
		log.Println("manager: self-update and binary onboarding disabled: -agent-binary not set")
	} else if catalog, err := BuildAgentCatalog(cfg.AgentBinaries); err != nil {
		log.Printf("manager: self-update and binary onboarding disabled: %v", err)
	} else {
		s.agents = catalog
		for _, b := range catalog.builds {
			log.Printf("manager: serving agent build %s (%s)", b.platform(), b.Path)
		}
	}
	if s.app = loadApp(cfg.AppAPK); s.app != nil {
		log.Printf("manager: offering the Android app to app workers (%s)", s.app.SHA256[:12])
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
	return s.Submit(ctx, WorkloadSpec{Target: target, Command: command, Args: args, Capability: capability, Params: params, Requirements: req, RestartPolicy: restartPolicy})
}

// WorkloadSpec is everything one submission asks for. Inputs name stored
// artifacts (Name + SHA256; the size is filled in from the store).
type WorkloadSpec struct {
	Target        domain.NodeID
	Command       string
	Args          []string
	Capability    domain.CapabilityName
	Params        map[string]string
	Requirements  domain.ResourceRequirements
	RestartPolicy domain.RestartPolicy
	Inputs        []domain.ArtifactRef
	Outputs       []string
	// Set only for a batch job's attempts (jobs.go).
	Job        domain.JobID
	Task       string
	Attempt    int
	AvoidNodes []domain.NodeID
	// Priority orders the queue (queue.go); empty = normal.
	Priority domain.Priority
	// ExcludeNodes: never place it there (a spot check, spotcheck.go).
	ExcludeNodes []domain.NodeID
}

// ErrInvalidWorkload is a submission that can never be valid as given
// (bad file names, unknown input artifact, files on a capability that
// can't take them).
var ErrInvalidWorkload = errors.New("manager: invalid workload")

// Submit is SubmitWorkload taking a full WorkloadSpec.
func (s *Server) Submit(ctx context.Context, spec WorkloadSpec) (domain.Workload, error) {
	if spec.Capability == "" {
		spec.Capability = domain.CapabilitySystemExecute
	}
	if err := s.checkPolicy(spec.Capability); err != nil {
		return domain.Workload{}, err
	}
	if err := compileTyped(&spec.Capability, &spec.Command, &spec.Args, &spec.Params, spec.Inputs, &spec.Outputs, &spec.Requirements); err != nil {
		return domain.Workload{}, err
	}
	if err := s.checkContainer(spec.Capability, spec.Params, &spec.Requirements); err != nil {
		return domain.Workload{}, err
	}
	if t, ok := catalog.Lookup(spec.Capability); ok && t.TargetRequired && spec.Target == "" {
		return domain.Workload{}, fmt.Errorf("%w: %s is about one device's own %s: say which device (target)", ErrInvalidWorkload, t.Name, "models")
	}
	if err := s.prepareFiles(&spec); err != nil {
		return domain.Workload{}, err
	}
	timeout := s.policyFor(spec.Capability).MaxRuntimeSeconds
	target, command, args, capability, params, req, restartPolicy := spec.Target, spec.Command, spec.Args, spec.Capability, spec.Params, spec.Requirements, spec.RestartPolicy
	if spec.Attempt < 0 || (spec.Job == "") != (spec.Task == "") {
		return domain.Workload{}, fmt.Errorf("%w: job, task and attempt go together", ErrInvalidWorkload)
	}
	priority, err := domain.ParsePriority(string(spec.Priority))
	if err != nil {
		return domain.Workload{}, fmt.Errorf("%w: %v", ErrInvalidWorkload, err)
	}
	pinned := target != ""
	id, err := newRandomID()
	if err != nil {
		return domain.Workload{}, err
	}
	newWorkload := func(target domain.NodeID) domain.Workload {
		return domain.Workload{ID: domain.WorkloadID(id), Target: target, Pinned: pinned, Command: command, Args: args, Capability: capability, Params: params, Requirements: req, RestartPolicy: restartPolicy, Inputs: spec.Inputs, Outputs: spec.Outputs,
			Job: spec.Job, Task: spec.Task, Attempt: spec.Attempt, AvoidNodes: spec.AvoidNodes, ExcludeNodes: spec.ExcludeNodes, TimeoutSeconds: timeout, Priority: priority}
	}
	s.placeMu.Lock()
	rec, resolved, err := s.resolve(placementFor(newWorkload(target)), nil)
	if errors.Is(err, errNoRoom) {
		// Some ready node could run it, just not right now: queue it.
		w := newWorkload(target)
		wrec := s.Workloads.enqueue(w, time.Now().UTC())
		s.Workloads.setWaiting(w.ID, waitingReason(err))
		s.placeMu.Unlock()
		s.persistWorkloadRecord(wrec)
		log.Printf("workload.queued: %s (%s)", w.ID, err)
		s.publish(domain.EventWorkloadQueued, target, map[string]any{"workloadId": string(w.ID), "command": command, "capability": string(capability)})
		return w, nil
	}
	if err != nil {
		s.placeMu.Unlock()
		return domain.Workload{}, err
	}
	target = resolved

	w := newWorkload(target)
	status := domain.WorkloadStatus{ID: w.ID, Target: target, State: domain.WorkloadPending}

	wrec := s.Workloads.Put(w, status)
	s.placeMu.Unlock()
	s.persistWorkloadRecord(wrec)

	s.assign(ctx, rec.Conn, w)
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
func (s *Server) resolveWorkloadTarget(target domain.NodeID, capability domain.CapabilityName, req domain.ResourceRequirements, features ...string) (*NodeRecord, domain.NodeID, error) {
	return s.resolve(placement{target: target, capability: capability, req: req, features: features}, nil)
}

// placement is one "where can this run" question.
type placement struct {
	target     domain.NodeID // pinned node, or "" for any
	capability domain.CapabilityName
	req        domain.ResourceRequirements
	features   []string        // agent features required (requiredFeatures)
	avoid      []domain.NodeID // prefer other nodes (a job task's failed attempts)
	exclude    []domain.NodeID // never these (a spot check's original device)
	params     map[string]string
	inputs     []domain.ArtifactRef // among equals, prefer nodes holding them (locality.go)
}

func placementFor(w domain.Workload) placement {
	p := placement{capability: w.EffectiveCapability(), req: w.Requirements, features: requiredFeatures(w), avoid: w.AvoidNodes, exclude: w.ExcludeNodes, params: w.Params, inputs: w.Inputs}
	if w.Pinned {
		p.target = w.Target
	}
	return p
}

// unconstrained reports whether any node with the capability and a free
// slot would do — so if one such workload finds no room, none will.
func (p placement) unconstrained() bool {
	if t, ok := catalog.Lookup(p.capability); ok && (t.HasChoices() || t.MaxPerNode > 0) {
		return false // a node-specific miss says nothing about other work
	}
	return p.target == "" && p.req.IsEmpty() && len(p.features) == 0 && len(p.avoid) == 0 && len(p.exclude) == 0
}

// offers reports whether rec matches p's catalog parameters (e.g. has
// the requested model).
func offers(rec *NodeRecord, p placement) (bool, string) {
	t, ok := catalog.Lookup(p.capability)
	if !ok || !t.HasChoices() {
		return true, ""
	}
	var attrs map[string]string
	for _, c := range rec.Capabilities {
		if c.Name == p.capability {
			attrs = c.Attributes
		}
	}
	return t.Offers(attrs, p.params)
}

// resolve answers p. usage, if non-nil, is the reservations to place
// against (a dispatch pass's snapshot, kept current as it assigns);
// otherwise each node's is derived fresh.
func (s *Server) resolve(p placement, usage map[domain.NodeID]nodeUsage) (*NodeRecord, domain.NodeID, error) {
	usageOf := func(id domain.NodeID) nodeUsage {
		if usage != nil {
			return usage[id]
		}
		return s.Workloads.usageOn(id)
	}
	if p.target != "" {
		rec, ok := s.Registry.Get(p.target)
		if !ok || rec.Conn == nil || rec.State != domain.NodeReady {
			return nil, "", ErrNodeNotConnected
		}
		if slices.Contains(p.exclude, p.target) {
			return nil, "", fmt.Errorf("%w (%s: %s)", ErrNoEligibleNode, p.target, excludedReason)
		}
		if ok, reason := couldEverFit(rec, p.capability, p.req, p.features...); !ok {
			return nil, "", fmt.Errorf("%w (%s: %s)", ErrNoEligibleNode, p.target, reason)
		}
		if sel := s.policyFor(p.capability).NodeLabels; !s.matchesLabels(p.target, sel) {
			return nil, "", fmt.Errorf("%w (%s: lacks the labels policy requires for %s (%v))", ErrNoEligibleNode, p.target, p.capability, sel)
		}
		if ok, reason := offers(rec, p); !ok {
			return nil, "", fmt.Errorf("%w (%s: %s)", ErrNoEligibleNode, p.target, reason)
		}
		notNow := func(reason string) error {
			return &noRoomError{detail: fmt.Sprintf("%s: %s", p.target, reason), reasons: []string{s.nodeDisplayName(p.target) + ": " + reason}}
		}
		if ok, reason := s.availableNow(rec, time.Now()); !ok {
			return nil, "", notNow(reason)
		}
		if ok, reason := nodeFits(rec, p.capability, p.req); !ok {
			return nil, "", notNow(reason)
		}
		u := usageOf(p.target)
		if ok, reason := hasRoom(rec, u, p.req); !ok {
			return nil, "", notNow(reason)
		}
		if ok, reason := perNodeRoom(u, p.capability); !ok {
			return nil, "", notNow(reason)
		}
		return rec, p.target, nil
	}

	var candidates []*NodeRecord
	for _, rec := range s.Registry.List() {
		if rec.State == domain.NodeReady && rec.Conn != nil {
			candidates = append(candidates, rec)
		}
	}
	if len(candidates) == 0 {
		return nil, "", ErrNoReadyNode
	}
	// First: could any ready node run it at all, once idle? If not,
	// reject. Nodes to avoid are dropped if any other could. Then: which
	// can take it right now (live state and free slots)? None → queue.
	var never []string
	var eligible, preferred []*NodeRecord
	selector := s.policyFor(p.capability).NodeLabels
	for _, rec := range candidates {
		id := rec.Node.Identity.NodeID
		if slices.Contains(p.exclude, id) {
			never = append(never, fmt.Sprintf("%s: %s", id, excludedReason))
			continue
		}
		if ok, reason := couldEverFit(rec, p.capability, p.req, p.features...); !ok {
			never = append(never, fmt.Sprintf("%s: %s", id, reason))
			continue
		}
		if !s.matchesLabels(id, selector) {
			never = append(never, fmt.Sprintf("%s: lacks the labels policy requires for %s (%v)", id, p.capability, selector))
			continue
		}
		if ok, reason := offers(rec, p); !ok {
			never = append(never, fmt.Sprintf("%s: %s", id, reason))
			continue
		}
		eligible = append(eligible, rec)
		if !slices.Contains(p.avoid, id) {
			preferred = append(preferred, rec)
		}
	}
	if len(eligible) == 0 {
		return nil, "", fmt.Errorf("%w (%v)", ErrNoEligibleNode, never)
	}
	if len(preferred) > 0 {
		eligible = preferred
	}
	var notNow, why []string
	var withRoom []*NodeRecord
	room := make(map[domain.NodeID]nodeUsage)
	now := time.Now()
	for _, rec := range eligible {
		id := rec.Node.Identity.NodeID
		skip := func(reason string) {
			notNow = append(notNow, fmt.Sprintf("%s: %s", id, reason))
			why = append(why, s.nodeDisplayName(id)+": "+reason)
		}
		if ok, reason := s.availableNow(rec, now); !ok {
			skip(reason)
			continue
		}
		if ok, reason := nodeFits(rec, p.capability, p.req); !ok {
			skip(reason)
			continue
		}
		u := usageOf(id)
		if ok, reason := hasRoom(rec, u, p.req); !ok {
			skip(reason)
			continue
		}
		if ok, reason := perNodeRoom(u, p.capability); !ok {
			skip(reason)
			continue
		}
		withRoom = append(withRoom, rec)
		room[id] = u
	}
	ranked := preferGPUFit(withRoom, p)
	best, err := selectNodeLocal(ranked, room, s.Registry.localBytes(ranked, p.inputs), p.capability, p.req)
	if err != nil {
		return nil, "", &noRoomError{detail: fmt.Sprint(notNow), reasons: why}
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
	if canceled, ok := s.Workloads.cancelQueued(id, "canceled while queued"); ok {
		s.forgetRequeues(id)
		s.persistWorkloadRecord(canceled)
		log.Printf("workload.canceled: %s (while queued)", id)
		s.publish(domain.EventWorkloadCanceled, canceled.Workload.Target, map[string]any{"workloadId": string(id), "reason": "canceled while queued"})
		return nil
	}
	rec, ok := s.Registry.Get(wrec.Workload.Target)
	if !ok || rec.Conn == nil {
		return ErrNodeNotConnected
	}
	s.Workloads.markCancelRequested(id)
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
		// Revocations load first so the seeding below can skip them.
		// RevokeNode forgets the node's record in the same transaction, so
		// a revoked-but-still-known node should never exist, but refusing
		// to resurrect one costs nothing.
		revoked, err := s.store.ListRevoked()
		if err != nil {
			return fmt.Errorf("manager: load revocations: %w", err)
		}
		for _, rev := range revoked {
			s.revocations.put(rev)
		}

		manifests, err := s.store.ListNodes()
		if err != nil {
			return fmt.Errorf("manager: load persisted nodes: %w", err)
		}
		for _, m := range manifests {
			if s.revocations.has(m.Node.Identity.NodeID) {
				continue
			}
			s.Registry.Seed(m)
		}

		metas, err := s.store.ListNodeMeta()
		if err != nil {
			return fmt.Errorf("manager: load node metadata: %w", err)
		}
		for id, meta := range metas {
			if !s.revocations.has(id) {
				s.meta.set(id, meta)
			}
		}

		workloads, err := s.store.ListWorkloads()
		if err != nil {
			return fmt.Errorf("manager: load persisted workloads: %w", err)
		}
		for _, pw := range workloads {
			s.Workloads.Seed(pw)
		}
		if p, found, err := s.store.GetPolicy(); err != nil {
			return fmt.Errorf("manager: load policy: %w", err)
		} else if found {
			s.policy.set(p)
		} else if s.cfg.InitialPolicy != nil {
			// First start with policy: record the starting point, so a
			// later restart can't silently change it.
			if err := s.store.PutPolicy(*s.cfg.InitialPolicy); err != nil {
				return fmt.Errorf("manager: persist policy: %w", err)
			}
		}
		jobs, err := s.store.ListJobs()
		if err != nil {
			return fmt.Errorf("manager: load persisted jobs: %w", err)
		}
		for _, j := range jobs {
			s.jobs.put(j)
		}
		workflows, err := s.store.ListWorkflows()
		if err != nil {
			return fmt.Errorf("manager: load persisted workflows: %w", err)
		}
		for _, wf := range workflows {
			s.workflows.put(wf)
		}
	}

	s.openJoinWindowOnFirstRun()

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
			s.flushSuppressed()
			for _, r := range s.joinReqs.prune() {
				s.publish(domain.EventJoinDecided, r.NodeID, map[string]any{"decision": "expired"})
			}
			for _, id := range s.Registry.ExpireStale(s.cfg.HeartbeatTimeout) {
				log.Printf("node.offline: %s", id)
				s.publish(domain.EventNodeOffline, id, map[string]any{"reason": "heartbeat timeout"})
				s.failPendingCommandsFor(id, "node went offline: heartbeat timeout")
				s.failWorkloadsFor(ctx, id, "node went offline: heartbeat timeout")
				// Then drop its connection. Only a REGISTER makes a node
				// READY again, so a node whose connection outlived the
				// silence (seen when the manager's own machine slept: its
				// local agent's connection survived and kept heartbeating)
				// would otherwise stay OFFLINE for good. A live agent
				// reconnects and registers within seconds. (After the
				// cancel above, which still uses this connection.)
				if conn := s.Registry.ConnOf(id); conn != nil {
					go conn.Close()
				}
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
			// The manager shutting down is not the node going away: its
			// in-flight work stays PENDING/RUNNING on disk and is seeded
			// UNKNOWN next start, rather than recorded as a failure the
			// work never had (which would cost a job task an attempt).
			if ctx.Err() == nil {
				s.failWorkloadsFor(ctx, nodeID, "node went offline: connection closed")
			}
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

		// RevokeNode closes a revoked node's connection, but a message
		// already in flight could still arrive first: drop the connection
		// rather than act on it (e.g. a late WORKLOAD_STATUS overwriting
		// the revocation's CANCELED/FAILED outcome).
		if nodeID != "" && s.revocations.has(nodeID) {
			return
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
		// Unverified: the claimed ID is recorded as a claim, never as the
		// entry's NodeID (anyone can claim any ID).
		s.auditRejection("node.rejected", "", conn.RemoteAddr(), "identity mismatch", map[string]any{"claimedNodeId": clip(string(claimedID))})
		return ""
	}

	// Verify proof of possession of the private key for that public key.
	// Without this, knowing a valid pairing token would be enough to
	// register as ANY existing NodeID.
	signed := protocol.RegisterSignedData(payload.PairingToken, claimedID)
	if !identity.Verify(node.Identity, signed, payload.Signature) {
		s.reject(ctx, conn, env.Source, "invalid signature: proof of key possession failed")
		s.auditRejection("node.rejected", "", conn.RemoteAddr(), "invalid signature", map[string]any{"claimedNodeId": clip(string(claimedID))})
		return ""
	}
	// From here on claimedID is verified: derived from the public key, and
	// the signature proves possession of its private key. (env.Source is
	// still whatever the peer wrote, so it never reaches the audit log.)

	// Held from the revocation check through persisting the admission, so
	// RevokeNode can't interleave between "allowed" and "registered" (see
	// admitMu). Registrations are rare enough that one lock is fine.
	s.admitMu.Lock()
	// A revoked identity is refused before any credential is considered,
	// even the shared pairing token: revocation exists precisely for
	// devices whose launcher still holds a valid one.
	if s.revocations.has(claimedID) {
		s.admitMu.Unlock()
		s.reject(ctx, conn, env.Source, revokedReason)
		s.auditRejection("node.rejected", claimedID, conn.RemoteAddr(), revokedReason, nil)
		return ""
	}

	// Admission credentials are required only for a previously unknown
	// identity. Once admitted, the persistent Ed25519 key is the durable
	// credential: a reconnect still has to pass the proof-of-possession
	// check above, but does not need to reuse a one-time enrollment token.
	_, knownNode := s.Registry.Get(claimedID)
	admittedBy := "known-identity"
	if !knownNode {
		switch {
		// First, before any token comparison: a pairing device sends no
		// token, and an empty configured token must never admit it.
		case payload.Pairing:
			code := protocol.PairingCode(s.cfg.Fingerprint, node.Identity.PublicKey)
			decision, isNew := s.joinReqs.request(JoinRequest{
				NodeID: claimedID, Name: clip(node.Name), Hostname: clip(node.Hostname),
				Platform: domain.Platform{OS: clip(node.Platform.OS), Architecture: clip(node.Platform.Architecture)},
				Remote:   remoteHost(conn.RemoteAddr()), Code: code,
			}, s.joinWindowOpen())
			if decision != joinApproved {
				s.admitMu.Unlock()
				s.answerJoinRequest(ctx, conn, env.Source, claimedID, decision, isNew, code, node.Name)
				return ""
			}
			admittedBy = "approval:" + code
		case payload.PairingToken == s.cfg.PairingToken:
			admittedBy = "pairing-token"
		case s.enrollments.consume(payload.PairingToken):
			admittedBy = "enrollment:" + tokenPrefix(payload.PairingToken)
		default:
			s.admitMu.Unlock()
			s.reject(ctx, conn, env.Source, "invalid pairing token")
			s.auditRejection("node.rejected", claimedID, conn.RemoteAddr(), "invalid pairing token", nil)
			return ""
		}
	}

	// Upsert leaves the node CONNECTED: nothing may be sent on the new
	// connection until REGISTER_ACK has gone out (below), or the agent —
	// still waiting for its answer — would read a command or a workload
	// first and drop the connection. Everything that sends work or
	// commands waits for READY.
	_, isNew := s.Registry.Upsert(payload.Manifest, conn)
	s.Registry.SetUse(claimedID, payload.Use)
	s.Registry.setCached(claimedID, payload.Cache)

	if s.store != nil {
		if err := s.store.UpsertNode(payload.Manifest); err != nil {
			log.Printf("manager: failed to persist node %s: %v", claimedID, err)
		}
	}
	s.admitMu.Unlock()

	s.send(ctx, conn, protocol.MsgRegisterAck, domain.ManagerNodeID, claimedID,
		protocol.RegisterAckPayload{NodeID: claimedID, ServerTime: time.Now().UTC()})
	s.Registry.SetState(claimedID, domain.NodeReady)

	// Recorded outside admitMu, so admission never holds the lock across
	// an extra disk write. Routine reconnects go to the noise log.
	detail := map[string]any{"by": admittedBy, "name": clip(node.Name), "hostname": clip(node.Hostname), "remote": conn.RemoteAddr()}
	if admittedBy == "known-identity" {
		s.audit(domain.AuditNoise, "node.reconnected", claimedID, actorNode, detail)
	} else {
		s.audit(domain.AuditAdmissions, "node.admitted", claimedID, actorNode, detail)
	}

	if isNew {
		log.Printf("node.registered: %s (%s)", claimedID, node.Name)
		s.publish(domain.EventNodeRegistered, claimedID, map[string]any{"name": node.Name})
	} else {
		log.Printf("node.reconnected: %s (%s)", claimedID, node.Name)
		s.publish(domain.EventNodeReconnected, claimedID, map[string]any{"name": node.Name})
	}
	s.publish(domain.EventNodeReady, claimedID, nil)
	s.kickDispatch() // new capacity: queued work may fit now

	return claimedID
}

// answerJoinRequest answers a pairing REGISTER that isn't admitted (yet).
// A waiting device asks again every couple of seconds, so only a new
// request is logged and published — never each retry (the phone's log is
// small and rotated).
func (s *Server) answerJoinRequest(ctx context.Context, conn domain.Conn, dest, id domain.NodeID, d joinDecision, isNew bool, code, name string) {
	switch d {
	case joinPending:
		if isNew {
			log.Printf("manager: join request from %s (%s) at %s, code %s: approve it on the dashboard", id, clip(name), conn.RemoteAddr(), code)
			s.publish(domain.EventJoinRequested, id, map[string]any{"name": clip(name), "code": code})
		}
		s.send(ctx, conn, protocol.MsgRegisterReject, domain.ManagerNodeID, dest, protocol.RegisterRejectPayload{
			Reason: "waiting for approval on the manager", PendingApproval: true, Code: code,
		})
	case joinRejected:
		s.send(ctx, conn, protocol.MsgRegisterReject, domain.ManagerNodeID, dest,
			protocol.RegisterRejectPayload{Reason: "the manager's operator declined this device"})
	case joinClosed:
		// Quiet: such a device asks again every few seconds until someone
		// opens the window.
		s.send(ctx, conn, protocol.MsgRegisterReject, domain.ManagerNodeID, dest,
			protocol.RegisterRejectPayload{Reason: "the manager isn't accepting new devices right now", JoinClosed: true})
	default: // joinFull
		s.reject(ctx, conn, dest, "too many devices are waiting for approval on the manager; approve or reject them first")
	}
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
	before := true
	if rec, ok := s.Registry.Get(nodeID); ok {
		before, _ = s.availableNow(rec, time.Now())
	}
	s.Registry.RecordHeartbeat(nodeID, payload.RuntimeState)
	s.Registry.setCached(nodeID, payload.Cache)
	s.noteAvailability(nodeID, before)
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

	// An agent may only report the states an execution actually passes
	// through. Anything else is a buggy or hostile agent; refusing it keeps
	// arbitrary strings out of persisted state and out of the dashboard,
	// which holds the operator's credential.
	switch payload.Status.State {
	case domain.WorkloadRunning, domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled:
	default:
		log.Printf("manager: ignoring workload status from %s with invalid state %q", nodeID, payload.Status.State)
		return
	}

	// A finished workload stays finished: a report that it is running
	// (a late progress update, say) must not bring it back — it would
	// hold its slot forever.
	switch current.Status.State {
	case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled, domain.WorkloadUnknown:
		if payload.Status.State == domain.WorkloadRunning {
			return
		}
	}
	// RUNNING again while RUNNING is a streaming task's output so far:
	// shown live, not written to disk every second.
	if current.Status.State == domain.WorkloadRunning && payload.Status.State == domain.WorkloadRunning {
		payload.Status.Outputs = nil
		s.Workloads.UpdateStatus(payload.Status)
		s.publish(domain.EventWorkloadProgress, nodeID, map[string]any{"workloadId": string(payload.Status.ID)})
		return
	}

	// Refused only for lack of a free slot (the manager's view raced the
	// agent's): re-queue rather than fail. Only agents that never ran it
	// can mark a refusal retryable, so a real failure is never retried here.
	if payload.Status.State == domain.WorkloadFailed && payload.Status.Retryable && payload.Status.StartedAt.IsZero() {
		s.handleBusyRefusal(nodeID, payload.Status)
		return
	}

	s.settleOutputs(current.Workload, &payload.Status)
	wrec, _ := s.Workloads.UpdateStatus(payload.Status)
	s.persistWorkloadRecord(wrec)
	switch payload.Status.State {
	case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled:
		// A slot freed: this node can take work again, and the queue may move.
		s.Registry.holdOffBusy(nodeID, time.Time{})
		s.forgetRequeues(payload.Status.ID)
		s.kickDispatch()
	}

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
