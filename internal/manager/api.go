package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"home-harness/internal/domain"
)

// nodeView is the JSON shape returned for a node — a flattened,
// API-stable projection of NodeRecord, not the internal type itself.
type nodeView struct {
	NodeID       domain.NodeID       `json:"nodeId"`
	Name         string              `json:"name"`
	Hostname     string              `json:"hostname"`
	Platform     domain.Platform     `json:"platform"`
	AgentVersion string              `json:"agentVersion"`
	BinaryHash   string              `json:"binaryHash,omitempty"`
	State        domain.NodeState    `json:"state"`
	LastSeen     time.Time           `json:"lastSeen"`
	Metrics      domain.RuntimeState `json:"metrics"`
	// UpdateStatus is computed here, platform-aware (selfupdate.go), so
	// the dashboard and harnessctl show the same verdict POST
	// /nodes/{id}/update acts on instead of each re-deriving it.
	UpdateStatus UpdateStatus `json:"updateStatus"`
	// Slots is how many workloads the node runs at once; Running is how
	// many of them are in use (PENDING/RUNNING on it) — see queue.go.
	Slots   int `json:"slots"`
	Running int `json:"running"`
	// Operator metadata (fleet.go) and same-machine hints. SameHostAs
	// lists other identities reporting this node's host fingerprint;
	// HostConflict means they match by fingerprint only, not hardware.
	Alias                 string            `json:"alias,omitempty"`
	Labels                map[string]string `json:"labels,omitempty"`
	HostFingerprintSource string            `json:"hostFingerprintSource,omitempty"`
	SameHostAs            []domain.NodeID   `json:"sameHostAs,omitempty"`
	HostConflict          bool              `json:"hostConflict,omitempty"`
	// Availability: the operator's rule, and whether the node takes new
	// work right now and why not (availability.go).
	Availability availabilityView `json:"availability"`
	// GPUs the device reported.
	GPUs []domain.GPU `json:"gpus,omitempty"`
}

func (s *Server) toNodeView(rec *NodeRecord, relations map[domain.NodeID]hostRelation) nodeView {
	meta := s.meta.get(rec.Node.Identity.NodeID)
	rel := relations[rec.Node.Identity.NodeID]
	return nodeView{
		NodeID:       rec.Node.Identity.NodeID,
		Name:         rec.Node.Name,
		Hostname:     rec.Node.Hostname,
		Platform:     rec.Node.Platform,
		AgentVersion: rec.Node.AgentVersion,
		BinaryHash:   rec.Node.BinaryHash,
		State:        rec.State,
		LastSeen:     rec.LastSeen,
		Metrics:      rec.LastMetrics,
		UpdateStatus: s.UpdateStatusFor(rec),
		Slots:        rec.slots(),
		Running:      s.Workloads.usageOn(rec.Node.Identity.NodeID).running,

		Alias:                 meta.Alias,
		Labels:                meta.Labels,
		HostFingerprintSource: rec.Node.HostFingerprintSource,
		SameHostAs:            rel.sameHostAs,
		HostConflict:          rel.conflict,
		Availability:          s.availabilityView(rec.Node.Identity.NodeID),
		GPUs:                  rec.GPUs,
	}
}

// NewHTTPHandler returns the manager's observability/control HTTP API
// (v1.md §15 "Basic API"):
//
//	GET  /nodes                    list all known nodes
//	GET  /nodes/{id}                one node's identity/state/metrics
//	GET  /nodes/{id}/resources       one node's declared resources
//	GET  /nodes/{id}/capabilities    one node's declared capabilities
//	GET  /resources/total           resource totals summed across all nodes
//	GET  /events                    Server-Sent Events stream of harness events
//	POST /nodes/{id}/commands        dispatch a command: {"name":"...","args":{...},"timeoutMs":...}
//	POST /nodes/{id}/revoke          permanently refuse a node identity: forget it, close its
//	                                 connection, and reject its REGISTER even with a valid
//	                                 pairing token (revocation.go)
//	GET  /revocations                list revoked node identities
//	DELETE /revocations/{id}         lift a revocation; the node must then be admitted afresh
//	POST /nodes/{id}/update          push a self-update if the node isn't already current
//	GET  /agent-binaries             the loaded agent-build catalog: [{os, architecture, sha256, path}]
//	GET  /agent-binary/hash          the legacy primary build's hash/platform (older clients)
//	GET  /join-info                  what a new node needs to onboard (fingerprint, pairing token, ...)
//	GET  /join-script                the ready-to-paste onboarding script for ?addr=&platform=
//	POST /enrollments                create a short-lived LAN or public-relay QR/link invitation
//	GET  /enrollments/{token}/qr     render an invitation URL as a self-contained SVG QR code
//	POST /login                      exchange the operator token for a dashboard session (operatorauth.go)
//	GET  /server-proof?nonce=        prove this manager holds the operator token, before a client sends it (operatorauth.go)
//	GET  /join-requests              devices waiting to be approved (agent -pair), with their pairing codes (joinrequests.go)
//	POST /join-requests/{id}/approve admit that device at its next connect; /reject refuses it for a while
//	GET  /join-window                whether new devices may join now; POST {"minutes":N} opens it, DELETE closes it (joinwindow.go)
//	PUT  /nodes/{id}/meta            set the operator's alias/labels for a node (fleet.go)
//	PUT  /nodes/{id}/availability    when the node takes new work: {"mode":"auto|always|idle|charging|paused","idleMinutes":N,"hours":"22:00-07:00"} (availability.go)
//	GET  /audit?log=&limit=          the audit log, newest first: log=security (default) or noise (audit.go)
//	GET  /                           a local web dashboard (node list + "add a device" form)
//	GET  /live                       the one-screen live view (devices, running work, events)
//	POST /workloads                 submit a workload: {"target":"...optional...","command":"...","args":[...],"requirements":{...optional...}}
//	GET  /workloads                 list all known workloads
//	GET  /workloads/{id}             one workload's request + status
//	POST /workloads/{id}/cancel      request cancellation of a running workload
//	POST /artifacts                  store a file (raw body; optional X-Artifact-SHA256) for workload inputs (artifacts.go)
//	GET  /artifacts                  list stored files and store usage
//	GET  /artifacts/{sha}[?name=]    download a stored file (always as an attachment)
//	DELETE /artifacts/{sha}          delete a stored file no live workload needs
//	GET  /catalog                    the typed task types, their schemas, policy, and how many nodes offer each (policy.go)
//	GET  /policy, PUT /policy        what the fleet may run: per-type enable, node labels, max runtime
//	GET  /models                     the local AI models READY nodes have, and where
//	GET  /ai-devices                 devices whose Ollama answers: their models (with sizes) and GPUs (aidevices.go)
//	GET  /llm/split                  the split session (a model run across devices with llama.cpp); POST {"name","model","main","helpers"} starts one, DELETE stops it (split.go)
//	POST /jobs                       submit a batch job: {"tasks":[...],"reduce":{...},"maxAttempts":3} (jobs.go)
//	GET  /jobs                       list jobs with progress counts
//	GET  /jobs/{id}                  one job: every task's state, attempts, node, outputs
//	POST /jobs/{id}/cancel           cancel a job and its in-flight attempts
//	GET  /ai-info                    the OpenAI-compatible API's base URL and AI key, for setting up apps
//	GET  /v1/models                  OpenAI-compatible: the models READY devices can chat with (openai.go)
//	POST /v1/chat/completions        OpenAI-compatible chat, streamed or not; also takes the AI key
//
// It is a thin adapter over Registry/SendCommand/Events — the manager's
// core logic has no HTTP dependency of its own.
//
// This API binds to loopback by default (see cmd/manager's -api-addr help
// text): POST /workloads inherits that default and it is now load-bearing
// in a way it wasn't for the harmless v0 command set — widening -api-addr
// exposes unauthenticated arbitrary code execution on every registered
// node, not just PING/ECHO. Loopback alone does not keep out a browser on
// this machine, so every route is also wrapped in guardOperatorAPI
// (apiguard.go) against cross-site requests and DNS rebinding.
func (s *Server) NewHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nodes", s.apiListNodes)
	mux.HandleFunc("GET /nodes/{id}", s.apiGetNode)
	mux.HandleFunc("GET /nodes/{id}/resources", s.apiGetNodeResources)
	mux.HandleFunc("GET /nodes/{id}/capabilities", s.apiGetNodeCapabilities)
	mux.HandleFunc("GET /resources/total", s.apiGetTotalResources)
	mux.HandleFunc("GET /events", s.apiEvents)
	mux.HandleFunc("POST /nodes/{id}/commands", s.apiPostCommand)
	mux.HandleFunc("POST /nodes/{id}/update", s.apiPostUpdate)
	mux.HandleFunc("POST /nodes/{id}/revoke", s.apiRevokeNode)
	mux.HandleFunc("GET /revocations", s.apiListRevocations)
	mux.HandleFunc("DELETE /revocations/{id}", s.apiUnrevokeNode)
	mux.HandleFunc("GET /agent-binaries", s.apiListAgentBinaries)
	mux.HandleFunc("GET /agent-binary/hash", s.apiGetAgentBinaryHash)
	mux.HandleFunc("GET /join-info", s.apiGetJoinInfo)
	mux.HandleFunc("GET /join-script", s.apiGetJoinScript)
	mux.HandleFunc("POST /enrollments", s.apiCreateEnrollment)
	mux.HandleFunc("GET /enrollments/{token}/qr", s.apiGetEnrollmentQR)
	mux.HandleFunc("GET /{$}", s.apiGetDashboard)
	mux.HandleFunc("GET /live", s.apiGetLive)
	mux.HandleFunc("POST /workloads", s.apiPostWorkload)
	mux.HandleFunc("GET /workloads", s.apiListWorkloads)
	mux.HandleFunc("GET /workloads/{id}", s.apiGetWorkload)
	mux.HandleFunc("POST /workloads/{id}/cancel", s.apiCancelWorkload)
	mux.HandleFunc("POST /login", s.apiLogin)
	mux.HandleFunc("GET /server-proof", s.apiServerProof)
	mux.HandleFunc("GET /join-requests", s.apiListJoinRequests)
	mux.HandleFunc("GET /join-window", s.apiGetJoinWindow)
	mux.HandleFunc("POST /join-window", s.apiOpenJoinWindow)
	mux.HandleFunc("DELETE /join-window", s.apiCloseJoinWindow)
	mux.HandleFunc("POST /join-requests/{id}/approve", s.apiDecideJoinRequest(true))
	mux.HandleFunc("POST /join-requests/{id}/reject", s.apiDecideJoinRequest(false))
	mux.HandleFunc("PUT /nodes/{id}/meta", s.apiPutNodeMeta)
	mux.HandleFunc("PUT /nodes/{id}/availability", s.apiPutNodeAvailability)
	mux.HandleFunc("GET /audit", s.apiListAudit)
	mux.HandleFunc("POST /artifacts", s.apiPostArtifact)
	mux.HandleFunc("GET /artifacts", s.apiListArtifacts)
	mux.HandleFunc("GET /artifacts/{sha}", s.apiGetArtifact)
	mux.HandleFunc("DELETE /artifacts/{sha}", s.apiDeleteArtifact)
	mux.HandleFunc("GET /catalog", s.apiGetCatalog)
	mux.HandleFunc("GET /models", s.apiListModels)
	mux.HandleFunc("GET /ai-devices", s.apiListAIDevices)
	mux.HandleFunc("GET /llm/split", s.apiGetSplit)
	mux.HandleFunc("POST /llm/split", s.apiStartSplit)
	mux.HandleFunc("DELETE /llm/split", s.apiStopSplit)
	mux.HandleFunc("GET /policy", s.apiGetPolicy)
	mux.HandleFunc("PUT /policy", s.apiPutPolicy)
	mux.HandleFunc("POST /jobs", s.apiPostJob)
	mux.HandleFunc("GET /jobs", s.apiListJobs)
	mux.HandleFunc("GET /jobs/{id}", s.apiGetJob)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.apiCancelJob)
	mux.HandleFunc("GET /ai-info", s.apiGetAIInfo)
	mux.HandleFunc("GET /v1/models", s.apiOpenAIModels)
	mux.HandleFunc("POST /v1/chat/completions", s.apiOpenAIChat)
	// Host check and CSRF protection first (apiguard.go), then operator
	// authentication (operatorauth.go) — browser defenses and the
	// credential check are independent layers.
	return guardOperatorAPI(s.requireOperator(mux))
}

func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	recs := s.Registry.List()
	relations := hostRelations(recs)
	views := make([]nodeView, 0, len(recs))
	for _, rec := range recs {
		views = append(views, s.toNodeView(rec, relations))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) apiGetNode(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Registry.Get(domain.NodeID(r.PathValue("id")))
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.toNodeView(rec, hostRelations(s.Registry.List())))
}

func (s *Server) apiGetNodeResources(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Registry.Get(domain.NodeID(r.PathValue("id")))
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec.Resources)
}

func (s *Server) apiGetNodeCapabilities(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Registry.Get(domain.NodeID(r.PathValue("id")))
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec.Capabilities)
}

func (s *Server) apiGetTotalResources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Registry.TotalResources())
}

type commandRequest struct {
	Name      domain.CommandName `json:"name"`
	Args      map[string]string  `json:"args"`
	TimeoutMS int                `json:"timeoutMs"`
}

const defaultCommandTimeout = 5 * time.Second

func (s *Server) apiPostCommand(w http.ResponseWriter, r *http.Request) {
	nodeID := domain.NodeID(r.PathValue("id"))

	var req commandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	timeout := defaultCommandTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	result, err := s.SendCommand(r.Context(), nodeID, req.Name, req.Args, timeout)
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeNotConnected):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusGatewayTimeout)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// apiPostUpdate triggers a self-update (internal/agent/selfupdate.go) on
// one node — only when updateTarget says the node's own platform has a
// different build it can actually fetch. It reuses SendCommand's exact
// dispatch/wait/timeout machinery via domain.CommandSelfUpdate, so a 200
// here means "the agent acknowledged and started," not "the update
// finished": completion is only observable via the node going OFFLINE and
// reconnecting with a new BinaryHash (its updateStatus turns "current").
func (s *Server) apiPostUpdate(w http.ResponseWriter, r *http.Request) {
	nodeID := domain.NodeID(r.PathValue("id"))

	rec, ok := s.Registry.Get(nodeID)
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	if s.agents.empty() {
		http.Error(w, "manager: self-update disabled (-agent-binary not set)", http.StatusConflict)
		return
	}
	status, build := s.updateTarget(rec)
	switch status {
	case UpdateCurrent:
		writeJSON(w, http.StatusOK, map[string]string{"status": "already-up-to-date"})
		return
	case UpdateUnknown:
		if rec.Node.BinaryHash == "" {
			http.Error(w, "node has not reported its agent binary hash, so there is nothing to compare an update against", http.StatusConflict)
		} else {
			http.Error(w, fmt.Sprintf("no agent build for %s/%s is loaded — restart the manager with an -agent-binary for that platform", rec.Node.Platform.OS, rec.Node.Platform.Architecture), http.StatusConflict)
		}
		return
	case UpdateReinstallRequired:
		http.Error(w, "this agent predates per-platform updates and can only download another platform's build; reinstall it once (e.g. with a fresh invitation), after which it updates normally", http.StatusConflict)
		return
	}

	// "path" names the per-platform catalog route; an agent from before
	// per-platform updates ignores it and fetches /agent-binary, which
	// updateTarget only allows when that serves this same build.
	result, err := s.SendCommand(r.Context(), nodeID, domain.CommandSelfUpdate,
		map[string]string{"sha256": build.SHA256, "path": build.downloadPath()}, defaultCommandTimeout)
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeNotConnected):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusGatewayTimeout)
		}
		return
	}
	s.audit(domain.AuditSecurity, "agent.update-dispatched", nodeID, actorFrom(r.Context()), map[string]any{
		"platform": build.platform(), "from": rec.Node.BinaryHash, "to": build.SHA256,
	})
	writeJSON(w, http.StatusOK, result)
}

// apiListAgentBinaries returns the loaded agent-build catalog.
func (s *Server) apiListAgentBinaries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.agents.views())
}

// apiGetAgentBinaryHash returns the legacy primary build's hash (empty if
// no builds are loaded) and platform. Kept for clients that predate the
// catalog; current clients read GET /agent-binaries, and node listings
// carry a server-computed updateStatus.
func (s *Server) apiGetAgentBinaryHash(w http.ResponseWriter, r *http.Request) {
	goos, arch := s.AgentBinaryPlatform()
	writeJSON(w, http.StatusOK, map[string]string{
		"sha256":       s.AgentBinaryHash(),
		"os":           goos,
		"architecture": arch,
	})
}

// apiGetJoinInfo returns what a new node needs to onboard itself — see
// JoinInfo (join.go) and cmd/harnessctl's `join` command.
func (s *Server) apiGetJoinInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.JoinInfo())
}

// workloadSummaryView is the JSON shape for a workload in a list — request
// identity plus state, deliberately excluding captured stdout/stderr so
// listing every workload can't balloon into megabytes of output (each
// individually already capped at domain.OutputCapBytes, but summed across
// many workloads that adds up).
type workloadSummaryView struct {
	ID       domain.WorkloadID    `json:"id"`
	Target   domain.NodeID        `json:"target"`
	Command  string               `json:"command"`
	Args     []string             `json:"args,omitempty"`
	State    domain.WorkloadState `json:"state"`
	ExitCode *int                 `json:"exitCode,omitempty"`
	// Capability is only set (and shown) for a non-system.execute
	// invocation — otherwise Command/Args above already say everything,
	// and every pre-v4 workload continues to show exactly as before.
	Capability domain.CapabilityName `json:"capability,omitempty"`
	// Waiting says why a QUEUED workload hasn't started yet.
	Waiting string `json:"waiting,omitempty"`
}

// workloadView is the JSON shape for a single workload — the summary plus
// its full captured output.
type workloadView struct {
	workloadSummaryView
	Requirements  domain.ResourceRequirements `json:"requirements,omitempty"`
	RestartPolicy domain.RestartPolicy        `json:"restartPolicy,omitempty"`
	RestartCount  int                         `json:"restartCount,omitempty"`
	NextRestartAt time.Time                   `json:"nextRestartAt,omitempty"`
	Params        map[string]string           `json:"params,omitempty"`
	Stdout        string                      `json:"stdout,omitempty"`
	Stderr        string                      `json:"stderr,omitempty"`
	Truncated     bool                        `json:"truncated,omitempty"`
	Error         string                      `json:"error,omitempty"`
	StartedAt     time.Time                   `json:"startedAt,omitempty"`
	FinishedAt    time.Time                   `json:"finishedAt,omitempty"`
	// Inputs and Outputs are the files the workload declared (as
	// submitted); OutputFiles are the outputs it actually delivered,
	// downloadable from GET /artifacts/{sha}.
	Inputs      []domain.ArtifactRef `json:"inputs,omitempty"`
	Outputs     []string             `json:"outputs,omitempty"`
	OutputFiles []domain.ArtifactRef `json:"outputFiles,omitempty"`
}

// exitCode returns a pointer to the process's actual exit code, or nil if
// none was ever produced (still in progress, canceled, or rejected before
// a process was launched at all) — as a plain int with omitempty, exit
// code 0 would be indistinguishable from "no exit code" over JSON.
func exitCode(status domain.WorkloadStatus) *int {
	if status.StartedAt.IsZero() {
		return nil
	}
	if status.State != domain.WorkloadCompleted && status.State != domain.WorkloadFailed {
		return nil
	}
	ec := status.ExitCode
	return &ec
}

func toWorkloadSummaryView(rec WorkloadRecord) workloadSummaryView {
	var capability domain.CapabilityName
	if rec.Workload.Capability != domain.CapabilitySystemExecute {
		capability = rec.Workload.Capability
	}
	v := workloadSummaryView{
		ID:         rec.Workload.ID,
		Target:     rec.Workload.Target,
		Command:    rec.Workload.Command,
		Args:       rec.Workload.Args,
		State:      rec.Status.State,
		ExitCode:   exitCode(rec.Status),
		Capability: capability,
	}
	if rec.Status.State == domain.WorkloadQueued {
		v.Waiting = rec.waiting
	}
	return v
}

func toWorkloadView(rec WorkloadRecord) workloadView {
	view := workloadView{
		workloadSummaryView: toWorkloadSummaryView(rec),
		Requirements:        rec.Workload.Requirements,
		RestartPolicy:       rec.Workload.RestartPolicy,
		RestartCount:        rec.Restart.Count,
		Stdout:              rec.Status.Stdout,
		Stderr:              rec.Status.Stderr,
		Truncated:           rec.Status.Truncated,
		Error:               rec.Status.Error,
		StartedAt:           rec.Status.StartedAt,
		FinishedAt:          rec.Status.FinishedAt,
		Inputs:              rec.Workload.Inputs,
		Outputs:             rec.Workload.Outputs,
		OutputFiles:         rec.Status.Outputs,
	}
	// NextRestartAt is only meaningful while the reconciler would actually
	// act on it — once a workload leaves restart eligibility (e.g. CANCELED
	// permanently excludes it, see RestartPolicy.WantsRestartAfter), the
	// value left over from its last-scheduled attempt is stale and would
	// otherwise display as if a restart were still pending forever.
	if rec.Workload.RestartPolicy.WantsRestartAfter(rec.Status.State) {
		view.NextRestartAt = rec.Restart.NextRestartAt
	}
	// Capability itself is already set on the embedded summary view above
	// (toWorkloadSummaryView) — Params is detail-only (excluded from the
	// list view, like Stdout/Stderr).
	if view.Capability != "" {
		view.Params = rec.Workload.Params
	}
	return view
}

type workloadRequest struct {
	Target        domain.NodeID               `json:"target"`
	Command       string                      `json:"command"`
	Args          []string                    `json:"args"`
	Capability    string                      `json:"capability,omitempty"`
	Params        map[string]string           `json:"params,omitempty"`
	Requirements  domain.ResourceRequirements `json:"requirements,omitempty"`
	RestartPolicy string                      `json:"restartPolicy,omitempty"`
	// Inputs name stored artifacts (POST /artifacts) to place in the
	// workload's working directory; Outputs are files it must leave there.
	Inputs []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"inputs,omitempty"`
	Outputs []string `json:"outputs,omitempty"`
}

func (s *Server) apiPostWorkload(w http.ResponseWriter, r *http.Request) {
	var req workloadRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Command is only required for the default system.execute capability —
	// any other capability carries its input in Params instead (e.g.
	// filesystem.read's "path"). The manager stays capability-agnostic
	// here: it doesn't validate Params' shape, only whether the target
	// declares the capability at all (SubmitWorkload -> resolveWorkloadTarget).
	if req.Command == "" && (req.Capability == "" || req.Capability == string(domain.CapabilitySystemExecute)) {
		http.Error(w, "bad request: command is required", http.StatusBadRequest)
		return
	}
	restartPolicy, err := domain.ParseRestartPolicy(req.RestartPolicy)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	spec := WorkloadSpec{
		Target: req.Target, Command: req.Command, Args: req.Args, Capability: domain.CapabilityName(req.Capability),
		Params: req.Params, Requirements: req.Requirements, RestartPolicy: restartPolicy, Outputs: req.Outputs,
	}
	for _, in := range req.Inputs {
		spec.Inputs = append(spec.Inputs, domain.ArtifactRef{Name: in.Name, SHA256: in.SHA256})
	}
	wl, err := s.Submit(r.Context(), spec)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidWorkload):
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrPolicy):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrNodeNotConnected), errors.Is(err, ErrNoReadyNode), errors.Is(err, ErrNoEligibleNode):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// The command and argument count only, never argv: arguments can carry
	// secrets, and truncating them wouldn't remove those.
	s.audit(domain.AuditSecurity, "workload.submitted", wl.Target, actorFrom(r.Context()), map[string]any{
		"workloadId": string(wl.ID), "command": wl.Command, "capability": string(wl.EffectiveCapability()), "args": len(wl.Args),
		"inputs": len(wl.Inputs), "outputs": len(wl.Outputs),
	})
	// The workload plus its state: QUEUED when every eligible node is full
	// right now (it starts as soon as one has room), else PENDING.
	state := domain.WorkloadPending
	if rec, ok := s.Workloads.Get(wl.ID); ok {
		state = rec.Status.State
	}
	writeJSON(w, http.StatusAccepted, struct {
		domain.Workload
		State domain.WorkloadState `json:"state"`
	}{wl, state})
}

func (s *Server) apiListWorkloads(w http.ResponseWriter, r *http.Request) {
	recs := s.Workloads.List()
	views := make([]workloadSummaryView, 0, len(recs))
	for _, rec := range recs {
		views = append(views, toWorkloadSummaryView(rec))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) apiGetWorkload(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Workloads.Get(domain.WorkloadID(r.PathValue("id")))
	if !ok {
		http.Error(w, "workload not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toWorkloadView(rec))
}

func (s *Server) apiCancelWorkload(w http.ResponseWriter, r *http.Request) {
	id := domain.WorkloadID(r.PathValue("id"))
	if err := s.CancelWorkload(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, ErrNodeNotConnected):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusNotFound)
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsubscribe := s.Events.Subscribe(16)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
