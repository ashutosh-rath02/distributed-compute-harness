package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

// Workload files (roadmap item 5).
//
// The operator uploads inputs to the content-addressed store
// (internal/artifacts) through the operator API and names them, with the
// outputs it expects, on a submission. Agents move a workload's files on
// their own agent-facing routes, authorized per assignment: every
// WORKLOAD_ASSIGN of a workload with files carries a fresh random token
// (never stored on the workload, persisted, or shown by the API) that is
// good only for that workload's declared inputs and outputs, only while
// it is PENDING/RUNNING on that node, and is dropped when it stops. Each
// declared output can be uploaded once per attempt, and only outputs the
// manager actually received are ever recorded — a COMPLETED report
// missing any is downgraded to FAILED.

const defaultArtifactRetention = 7 * 24 * time.Hour

// transferGrant is one assignment's authority to move its files.
type transferGrant struct {
	workload domain.WorkloadID
	node     domain.NodeID
	key      [32]byte
	uploads  map[string]domain.ArtifactRef // declared output name -> stored
	inFlight map[string]bool
	// reserved is how many bytes this attempt's uploads have claimed of
	// its budget (the store's per-file limit, for all outputs together).
	reserved int64
}

type grantTable struct {
	mu      sync.Mutex
	byWork  map[domain.WorkloadID]*transferGrant
	byToken map[[32]byte]*transferGrant
}

func newGrantTable() *grantTable {
	return &grantTable{byWork: make(map[domain.WorkloadID]*transferGrant), byToken: make(map[[32]byte]*transferGrant)}
}

// Tokens are looked up by their hash, so the lookup reveals nothing
// useful about a stored token through timing.
func tokenKey(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// mint issues a fresh token for workload's assignment to node, replacing
// any earlier attempt's.
func (g *grantTable) mint(workload domain.WorkloadID, node domain.NodeID) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	gr := &transferGrant{workload: workload, node: node, key: tokenKey(token), uploads: map[string]domain.ArtifactRef{}, inFlight: map[string]bool{}}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dropLocked(workload)
	g.byWork[workload] = gr
	g.byToken[gr.key] = gr
	return token, nil
}

func (g *grantTable) dropLocked(workload domain.WorkloadID) {
	if old, ok := g.byWork[workload]; ok {
		delete(g.byToken, old.key)
		delete(g.byWork, workload)
	}
}

func (g *grantTable) drop(workload domain.WorkloadID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dropLocked(workload)
}

func (g *grantTable) lookup(token string) (*transferGrant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr, ok := g.byToken[tokenKey(token)]
	return gr, ok
}

// sweep drops grants whose assignment is over (node lost, revoked,
// canceled, re-queued) — the request-time state check already refuses
// them; this just keeps the table from growing.
func (g *grantTable) sweep(live func(domain.WorkloadID, domain.NodeID) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, gr := range g.byWork {
		if !live(id, gr.node) {
			g.dropLocked(id)
		}
	}
}

func (g *grantTable) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.byWork)
}

var (
	errAlreadyUploaded = errors.New("this output was already uploaded for this attempt")
	errOverBudget      = errors.New("this attempt's outputs would exceed its upload budget")
)

// beginUpload claims name for one upload of size bytes in this attempt,
// out of a budget shared by all its outputs.
func (g *grantTable) beginUpload(gr *transferGrant, name string, size, budget int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, done := gr.uploads[name]; done || gr.inFlight[name] {
		return errAlreadyUploaded
	}
	if size < 0 || gr.reserved+size > budget {
		return errOverBudget
	}
	gr.inFlight[name] = true
	gr.reserved += size
	return nil
}

// finishUpload ends name's upload: ref is what was stored, or nil if it
// failed (its reservation is returned and it may be retried).
func (g *grantTable) finishUpload(gr *transferGrant, name string, size int64, ref *domain.ArtifactRef) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(gr.inFlight, name)
	if ref != nil {
		gr.uploads[name] = *ref
	} else {
		gr.reserved -= size
	}
}

// verifiedOutputs keeps only claimed outputs the manager received in
// this attempt, exactly as received, in declaration order.
func (g *grantTable) verifiedOutputs(w domain.Workload, claimed []domain.ArtifactRef) []domain.ArtifactRef {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr, ok := g.byWork[w.ID]
	if !ok {
		return nil
	}
	claims := make(map[string]domain.ArtifactRef, len(claimed))
	for _, c := range claimed {
		claims[c.Name] = c
	}
	var out []domain.ArtifactRef
	for _, name := range w.Outputs {
		got, uploaded := gr.uploads[name]
		c, claimedIt := claims[name]
		if uploaded && claimedIt && c.SHA256 == got.SHA256 && c.Size == got.Size {
			out = append(out, got)
		}
	}
	return out
}

// assign sends WORKLOAD_ASSIGN for w's current attempt, with a fresh
// transfer token when it declares files (and marks its inputs as in use,
// so garbage collection keeps them for another retention period).
func (s *Server) assign(ctx context.Context, conn domain.Conn, w domain.Workload) {
	payload := protocol.WorkloadAssignPayload{Workload: w}
	if w.HasFiles() {
		token, err := s.grants.mint(w.ID, w.Target)
		if err != nil {
			log.Printf("manager: mint transfer token for %s: %v", w.ID, err)
		}
		payload.ArtifactToken = token
		if s.cfg.Artifacts != nil {
			for _, in := range w.Inputs {
				s.cfg.Artifacts.Touch(in.SHA256)
			}
		}
	}
	s.send(ctx, conn, protocol.MsgWorkloadAssign, domain.ManagerNodeID, w.Target, payload)
}

// prepareFiles validates a submission's files and fills each input's size
// from the store.
func (s *Server) prepareFiles(spec *WorkloadSpec) error {
	if len(spec.Inputs) == 0 && len(spec.Outputs) == 0 {
		return nil
	}
	if spec.Capability != domain.CapabilitySystemExecute {
		return fmt.Errorf("%w: only %s workloads can take input or output files", ErrInvalidWorkload, domain.CapabilitySystemExecute)
	}
	if s.cfg.Artifacts == nil {
		return fmt.Errorf("%w: this manager has no artifact store", ErrInvalidWorkload)
	}
	if err := domain.ValidateWorkloadFiles(spec.Inputs, spec.Outputs); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidWorkload, err)
	}
	inputs := make([]domain.ArtifactRef, len(spec.Inputs))
	for i, in := range spec.Inputs {
		info, err := s.cfg.Artifacts.Stat(in.SHA256)
		if err != nil {
			return fmt.Errorf("%w: input %s: %v", ErrInvalidWorkload, in.Name, err)
		}
		s.cfg.Artifacts.Touch(in.SHA256)
		inputs[i] = domain.ArtifactRef{Name: in.Name, SHA256: in.SHA256, Size: info.Size}
	}
	spec.Inputs = inputs
	return nil
}

// requiredFeatures is what an agent must advertise to run w.
func requiredFeatures(w domain.Workload) []string {
	if w.HasFiles() {
		return []string{domain.FeatureArtifacts}
	}
	return nil
}

// settleOutputs replaces an agent's claimed outputs with the verified
// ones on a terminal report (dropping the assignment's grant), and strips
// any claim from a non-terminal one. A COMPLETED report the manager can't
// back with every declared output becomes FAILED.
func (s *Server) settleOutputs(w domain.Workload, status *domain.WorkloadStatus) {
	switch status.State {
	case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled:
	default:
		status.Outputs = nil
		return
	}
	if !w.HasFiles() {
		status.Outputs = nil
		return
	}
	status.Outputs = s.grants.verifiedOutputs(w, status.Outputs)
	s.grants.drop(w.ID)
	if status.State == domain.WorkloadCompleted && len(status.Outputs) != len(w.Outputs) {
		got := map[string]bool{}
		for _, o := range status.Outputs {
			got[o.Name] = true
		}
		var missing []string
		for _, name := range w.Outputs {
			if !got[name] {
				missing = append(missing, name)
			}
		}
		log.Printf("manager: workload %s reported COMPLETED without delivering output(s) %v; recording FAILED", w.ID, missing)
		status.State = domain.WorkloadFailed
		status.Error = "declared output(s) not received by the manager: " + strings.Join(missing, ", ")
	}
}

// liveArtifacts is every artifact some workload may still need: inputs of
// work that is queued, assigned, running, of unknown fate, or due to be
// restarted.
func (s *Server) liveArtifacts() map[string]bool {
	live := map[string]bool{}
	for _, rec := range s.Workloads.List() {
		switch rec.Status.State {
		case domain.WorkloadQueued, domain.WorkloadPending, domain.WorkloadRunning, domain.WorkloadUnknown:
		default:
			if !rec.Workload.RestartPolicy.WantsRestartAfter(rec.Status.State) {
				continue
			}
		}
		for _, in := range rec.Workload.Inputs {
			live[in.SHA256] = true
		}
	}
	return live
}

// collectArtifacts deletes stored artifacts unused for the retention
// period that no live workload needs.
func (s *Server) collectArtifacts() {
	if s.cfg.Artifacts == nil {
		return
	}
	retention := s.cfg.ArtifactRetention
	if retention <= 0 {
		retention = defaultArtifactRetention
	}
	live := s.liveArtifacts()
	if removed, freed := s.cfg.Artifacts.GC(time.Now().Add(-retention), func(sha string) bool { return live[sha] }); removed > 0 {
		log.Printf("manager: artifact GC removed %d file(s), %d bytes", removed, freed)
	}
}

// sweepGrants drops transfer grants whose assignment is no longer live.
func (s *Server) sweepGrants() {
	s.grants.sweep(func(id domain.WorkloadID, node domain.NodeID) bool {
		rec, ok := s.Workloads.Get(id)
		return ok && rec.Workload.Target == node &&
			(rec.Status.State == domain.WorkloadPending || rec.Status.State == domain.WorkloadRunning)
	})
}

// ---- agent-facing routes (registered on the agent transports by cmd/manager)

// ArtifactTransferHandler serves agents' workload file transfers:
//
//	GET /workload-artifacts/{workload}/inputs/{sha}
//	PUT /workload-artifacts/{workload}/outputs/{name...}
func (s *Server) ArtifactTransferHandler() http.HandlerFunc {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workload-artifacts/{workload}/inputs/{sha}", s.agentGetInput)
	mux.HandleFunc("PUT /workload-artifacts/{workload}/outputs/{name...}", s.agentPutOutput)
	return mux.ServeHTTP
}

// grantFor authorizes an agent transfer request for its workload.
func (s *Server) grantFor(w http.ResponseWriter, r *http.Request) (*transferGrant, WorkloadRecord, bool) {
	if s.cfg.Artifacts == nil {
		http.Error(w, "this manager has no artifact store", http.StatusNotFound)
		return nil, WorkloadRecord{}, false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, "missing transfer token", http.StatusUnauthorized)
		return nil, WorkloadRecord{}, false
	}
	gr, ok := s.grants.lookup(token)
	if !ok {
		http.Error(w, "unknown or expired transfer token", http.StatusUnauthorized)
		return nil, WorkloadRecord{}, false
	}
	if string(gr.workload) != r.PathValue("workload") {
		http.Error(w, "token is for a different workload", http.StatusForbidden)
		return nil, WorkloadRecord{}, false
	}
	rec, ok := s.Workloads.Get(gr.workload)
	if !ok || rec.Workload.Target != gr.node ||
		(rec.Status.State != domain.WorkloadPending && rec.Status.State != domain.WorkloadRunning) {
		http.Error(w, "this assignment is no longer active", http.StatusForbidden)
		return nil, WorkloadRecord{}, false
	}
	return gr, rec, true
}

func (s *Server) agentGetInput(w http.ResponseWriter, r *http.Request) {
	_, rec, ok := s.grantFor(w, r)
	if !ok {
		return
	}
	sha := r.PathValue("sha")
	if !domain.ValidSHA256(sha) {
		http.Error(w, artifacts.ErrInvalidID.Error(), http.StatusBadRequest)
		return
	}
	declared := false
	for _, in := range rec.Workload.Inputs {
		declared = declared || in.SHA256 == sha
	}
	if !declared {
		http.Error(w, "not an input of this workload", http.StatusForbidden)
		return
	}
	s.serveArtifact(w, sha, "")
}

func (s *Server) agentPutOutput(w http.ResponseWriter, r *http.Request) {
	gr, rec, ok := s.grantFor(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	declared := false
	for _, out := range rec.Workload.Outputs {
		declared = declared || out == name
	}
	if !declared {
		http.Error(w, "not a declared output of this workload", http.StatusForbidden)
		return
	}
	sha := r.Header.Get("X-Artifact-SHA256")
	if !domain.ValidSHA256(sha) {
		http.Error(w, "X-Artifact-SHA256 must be the content's lowercase hex sha256", http.StatusBadRequest)
		return
	}
	// The size is declared up front so it can be reserved against the
	// attempt's budget before a byte is stored.
	size := r.ContentLength
	if size < 0 {
		http.Error(w, "Content-Length is required", http.StatusLengthRequired)
		return
	}
	store := s.cfg.Artifacts
	switch err := s.grants.beginUpload(gr, name, size, store.MaxBytes()); {
	case errors.Is(err, errAlreadyUploaded):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	info, err := store.PutLimited(http.MaxBytesReader(w, r.Body, size+1), sha, size)
	if err == nil && info.Size != size {
		err = fmt.Errorf("%w: got %d bytes, Content-Length said %d", artifacts.ErrHashMismatch, info.Size, size)
	}
	if err != nil {
		s.grants.finishUpload(gr, name, size, nil)
		writeArtifactError(w, err)
		return
	}
	ref := domain.ArtifactRef{Name: name, SHA256: info.SHA256, Size: info.Size}
	s.grants.finishUpload(gr, name, size, &ref)
	writeJSON(w, http.StatusCreated, info)
}

// ---- operator API

func writeArtifactError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, artifacts.ErrTooLarge), errors.As(err, &maxErr):
		http.Error(w, artifacts.ErrTooLarge.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, artifacts.ErrStoreFull):
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
	case errors.Is(err, artifacts.ErrHashMismatch), errors.Is(err, artifacts.ErrInvalidID):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, artifacts.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) requireArtifactStore(w http.ResponseWriter) bool {
	if s.cfg.Artifacts == nil {
		http.Error(w, "this manager has no artifact store", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// POST /artifacts: the raw request body is the file. An optional
// X-Artifact-SHA256 header is verified against it.
func (s *Server) apiPostArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireArtifactStore(w) {
		return
	}
	store := s.cfg.Artifacts
	expect := r.Header.Get("X-Artifact-SHA256")
	if expect != "" && !domain.ValidSHA256(expect) {
		http.Error(w, artifacts.ErrInvalidID.Error(), http.StatusBadRequest)
		return
	}
	info, err := store.Put(http.MaxBytesReader(w, r.Body, store.MaxBytes()+1), expect)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "artifact.uploaded", "", actorFrom(r.Context()), map[string]any{"sha256": info.SHA256[:12], "size": info.Size})
	writeJSON(w, http.StatusCreated, info)
}

// GET /artifacts
func (s *Server) apiListArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.requireArtifactStore(w) {
		return
	}
	list, err := s.cfg.Artifacts.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []artifacts.Info{}
	}
	used, total := s.cfg.Artifacts.Usage()
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": list, "usedBytes": used, "totalBytes": total, "maxBytes": s.cfg.Artifacts.MaxBytes()})
}

// GET /artifacts/{sha}[?name=out/result.txt]
func (s *Server) apiGetArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireArtifactStore(w) {
		return
	}
	sha := r.PathValue("sha")
	if !domain.ValidSHA256(sha) {
		http.Error(w, artifacts.ErrInvalidID.Error(), http.StatusBadRequest)
		return
	}
	s.serveArtifact(w, sha, r.URL.Query().Get("name"))
}

// serveArtifact streams one artifact as an opaque download. Its bytes may
// come from an agent, and this origin holds the operator credential: the
// headers make sure no browser ever renders or sniffs it as a page.
func (s *Server) serveArtifact(w http.ResponseWriter, sha, name string) {
	f, info, err := s.cfg.Artifacts.Open(sha)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	defer f.Close()
	filename := sha
	if name != "" && domain.ValidArtifactName(name) == nil {
		filename = path.Base(name)
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
}

// DELETE /artifacts/{sha}: refused while a live workload needs it.
func (s *Server) apiDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireArtifactStore(w) {
		return
	}
	sha := r.PathValue("sha")
	if !domain.ValidSHA256(sha) {
		http.Error(w, artifacts.ErrInvalidID.Error(), http.StatusBadRequest)
		return
	}
	if s.liveArtifacts()[sha] {
		http.Error(w, "a queued, running, or restarting workload still needs this artifact", http.StatusConflict)
		return
	}
	if err := s.cfg.Artifacts.Delete(sha); err != nil {
		writeArtifactError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "artifact.deleted", "", actorFrom(r.Context()), map[string]any{"sha256": sha[:12]})
	w.WriteHeader(http.StatusNoContent)
}
