package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

// Split sessions (roadmap item 10): one model too big for any single
// device runs across several with llama.cpp. The main device (which has
// the model file) runs llama-server; each helper runs ggml-rpc-server on
// its loopback, reached through a tunnel (tunnel.go). A session is a set
// of ordinary, pinned workloads — llm.split-helper on each helper, then
// llm.split-main once they are ready — so placement, availability,
// keep-awake and the dashboard see them like any work. Chats reach the
// model through /v1 like any other: the main device offers it as an
// llm.chat model while the session runs.
//
// v1, deliberately narrow: one session at a time; devices on the LAN
// (tunnels refuse the relay); the same llama.cpp build on every part; the
// model file already on the main device; llama.cpp installed by the device
// owner (agents only detect it).
//
// A session ends — every part canceled, its tunnels revoked — when the
// operator stops it, any part ends or its device goes away, a helper stops
// being available (llama.cpp can't drop a helper mid-session, and a
// device's owner unplugging it or sitting down at it must win), or no chat
// used it for SplitIdleTimeout (its memory and keep-awake come back).

// SplitIdleTimeout ends a session nobody chatted with for this long.
var SplitIdleTimeout = 30 * time.Minute

const (
	capSplitHelper = domain.CapabilityName("llm.split-helper")
	capSplitMain   = domain.CapabilityName("llm.split-main")
	// splitHelperWait bounds helpers starting their servers.
	splitHelperWait = 2 * time.Minute
)

// SplitState is where a session is.
type SplitState string

const (
	SplitStarting SplitState = "starting" // helpers starting
	SplitLoading  SplitState = "loading"  // llama-server loading the model onto every part
	SplitReady    SplitState = "ready"    // answering chats
	SplitStopped  SplitState = "stopped"
)

// splitSession is the one session (in memory: a manager restart ends it).
type splitSession struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Model    string          `json:"model"`
	Main     domain.NodeID   `json:"main"`
	Helpers  []domain.NodeID `json:"helpers"`
	State    SplitState      `json:"state"`
	Reason   string          `json:"reason,omitempty"`
	Started  time.Time       `json:"started"`
	LastUsed time.Time       `json:"lastUsed"`

	helperWork []domain.WorkloadID
	mainWork   domain.WorkloadID
	// Tunnel grants, handed out with each part's assignment.
	mainGrants   []protocol.TunnelGrant
	helperGrants map[domain.NodeID]protocol.TunnelGrant
	stop         chan string
}

type splitTable struct {
	mu  sync.Mutex
	cur *splitSession
}

func newSplitTable() *splitTable { return &splitTable{} }

// splitGrants are the tunnel tokens w's assignment carries, if w is a
// part of the running session.
func (s *Server) splitGrants(w domain.Workload) []protocol.TunnelGrant {
	if s.splits == nil { // a bare Server{} (placement unit tests)
		return nil
	}
	s.splits.mu.Lock()
	defer s.splits.mu.Unlock()
	sess := s.splits.cur
	if sess == nil || sess.State == SplitStopped || w.Params["session"] != sess.ID {
		return nil
	}
	switch w.EffectiveCapability() {
	case capSplitHelper:
		if g, ok := sess.helperGrants[w.Target]; ok {
			return []protocol.TunnelGrant{g}
		}
	case capSplitMain:
		if w.Target == sess.Main {
			return append([]protocol.TunnelGrant(nil), sess.mainGrants...)
		}
	}
	return nil
}

// touchSplit records a chat with model, keeping its session from idling out.
func (s *Server) touchSplit(model string) {
	if s.splits == nil {
		return
	}
	s.splits.mu.Lock()
	defer s.splits.mu.Unlock()
	if sess := s.splits.cur; sess != nil && sess.State != SplitStopped && catalog.NormalizeModel(sess.Name) == catalog.NormalizeModel(model) {
		sess.LastUsed = time.Now()
	}
}

// SplitRequest starts a session.
type SplitRequest struct {
	Name    string          `json:"name"`    // what chats call the model
	Model   string          `json:"model"`   // the .gguf file on the main device
	Main    domain.NodeID   `json:"main"`    // the device with the model file
	Helpers []domain.NodeID `json:"helpers"` // empty: every other device that can help now
}

// ErrSplitBusy: one session at a time.
var ErrSplitBusy = errors.New("manager: a split session is already running")

// StartSplit validates a request and starts the session in the background.
func (s *Server) StartSplit(req SplitRequest) (*splitSession, error) {
	t, _ := catalog.Lookup(capSplitMain)
	if _, _, err := t.Compile(map[string]string{"session": "0000000000000000", "model": req.Model, "name": req.Name, "helpers": "1"}, nil); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWorkload, err)
	}
	mainRec, ok := s.Registry.Get(req.Main)
	if !ok || mainRec.State != domain.NodeReady || !offersVersion(mainRec, capSplitMain) {
		return nil, fmt.Errorf("%w: %s can't run a split model's main part (llama.cpp's llama-server installed? connected?)", ErrInvalidWorkload, req.Main)
	}
	if ok, why := s.availableNow(mainRec, time.Now()); !ok {
		return nil, fmt.Errorf("%w: %s isn't taking work now: %s", ErrNoEligibleNode, s.nodeDisplayName(req.Main), why)
	}
	version := attrValue(mainRec, capSplitMain, catalog.AttrLlamaVersion)
	helpers := req.Helpers
	if len(helpers) == 0 {
		for _, rec := range s.Registry.List() {
			id := rec.Node.Identity.NodeID
			if id == req.Main || rec.State != domain.NodeReady || !offersVersion(rec, capSplitHelper) {
				continue
			}
			if ok, _ := s.availableNow(rec, time.Now()); ok && attrValue(rec, capSplitHelper, catalog.AttrLlamaVersion) == version {
				helpers = append(helpers, id)
			}
		}
	}
	if len(helpers) == 0 {
		return nil, fmt.Errorf("%w: no other device can help (llama.cpp's ggml-rpc-server installed, same build %s, taking work)", ErrNoEligibleNode, version)
	}
	if len(helpers) > 15 {
		return nil, fmt.Errorf("%w: at most 15 helpers", ErrInvalidWorkload)
	}
	seen := map[domain.NodeID]bool{req.Main: true}
	for _, h := range helpers {
		rec, ok := s.Registry.Get(h)
		switch {
		case seen[h]:
			return nil, fmt.Errorf("%w: %s listed twice (or is the main device)", ErrInvalidWorkload, h)
		case !ok || rec.State != domain.NodeReady || !offersVersion(rec, capSplitHelper):
			return nil, fmt.Errorf("%w: %s can't help (llama.cpp's ggml-rpc-server installed? connected?)", ErrInvalidWorkload, h)
		case attrValue(rec, capSplitHelper, catalog.AttrLlamaVersion) != version:
			return nil, fmt.Errorf("%w: %s runs llama.cpp %s, the main device %s: every part needs the same build", ErrInvalidWorkload, s.nodeDisplayName(h), attrValue(rec, capSplitHelper, catalog.AttrLlamaVersion), version)
		}
		if ok, why := s.availableNow(rec, time.Now()); !ok {
			return nil, fmt.Errorf("%w: %s isn't taking work now: %s", ErrNoEligibleNode, s.nodeDisplayName(h), why)
		}
		seen[h] = true
	}
	id, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	sess := &splitSession{ID: id, Name: req.Name, Model: req.Model, Main: req.Main, Helpers: helpers, State: SplitStarting,
		Started: time.Now(), LastUsed: time.Now(), helperGrants: map[domain.NodeID]protocol.TunnelGrant{}, stop: make(chan string, 1)}
	for i, h := range helpers {
		mainTok, helperTok, err := s.OpenTunnelPair(id, i, req.Main, h)
		if err != nil {
			s.CloseTunnelSession(id)
			return nil, err
		}
		sess.mainGrants = append(sess.mainGrants, protocol.TunnelGrant{Session: id, Index: i, Token: mainTok})
		sess.helperGrants[h] = protocol.TunnelGrant{Session: id, Index: i, Token: helperTok}
	}
	s.splits.mu.Lock()
	if cur := s.splits.cur; cur != nil && cur.State != SplitStopped {
		s.splits.mu.Unlock()
		s.CloseTunnelSession(id)
		return nil, ErrSplitBusy
	}
	s.splits.cur = sess
	s.splits.mu.Unlock()
	go s.runSplit(sess)
	return sess, nil
}

// StopSplit ends the running session.
func (s *Server) StopSplit(reason string) bool {
	s.splits.mu.Lock()
	sess := s.splits.cur
	s.splits.mu.Unlock()
	if sess == nil {
		return false
	}
	select {
	case sess.stop <- reason:
	default:
	}
	return true
}

func (s *Server) setSplit(sess *splitSession, state SplitState, reason string) {
	s.splits.mu.Lock()
	sess.State, sess.Reason = state, reason
	s.splits.mu.Unlock()
	log.Printf("split %s (%s): %s %s", sess.ID, sess.Name, state, reason)
	s.publish(domain.EventNodeUpdated, sess.Main, map[string]any{"reason": "split session", "session": sess.ID, "state": string(state)})
}

// partOutput is a part's state and output so far.
func (s *Server) partOutput(id domain.WorkloadID) (domain.WorkloadState, string, string) {
	rec, ok := s.Workloads.Get(id)
	if !ok {
		return domain.WorkloadUnknown, "", "it disappeared"
	}
	return rec.Status.State, rec.Status.Stdout, rec.Status.Error
}

func (s *Server) runSplit(sess *splitSession) {
	ctx := context.Background()
	end := func(reason string) {
		for _, id := range append(append([]domain.WorkloadID(nil), sess.helperWork...), sess.mainWork) {
			if id == "" {
				continue
			}
			if st, _, _ := s.partOutput(id); !chatOver(st) {
				s.CancelWorkload(ctx, id)
			}
		}
		s.CloseTunnelSession(sess.ID)
		s.setSplit(sess, SplitStopped, reason)
	}
	submit := func(target domain.NodeID, capability domain.CapabilityName, params map[string]string) (domain.WorkloadID, error) {
		w, err := s.Submit(ctx, WorkloadSpec{Target: target, Capability: capability, Params: params})
		return w.ID, err
	}
	for i, h := range sess.Helpers {
		id, err := submit(h, capSplitHelper, map[string]string{"session": sess.ID, "index": strconv.Itoa(i)})
		if err != nil {
			end(fmt.Sprintf("couldn't start helper %s: %v", s.nodeDisplayName(h), err))
			return
		}
		sess.helperWork = append(sess.helperWork, id)
	}
	// wait polls every second until done() says so, a part ends, the
	// operator stops the session, or the deadline passes.
	wait := func(deadline time.Time, done func() bool) string {
		for !done() {
			for i, id := range sess.helperWork {
				if st, _, errText := s.partOutput(id); chatOver(st) {
					return fmt.Sprintf("helper %s ended: %s", s.nodeDisplayName(sess.Helpers[i]), orState(errText, st))
				}
			}
			if sess.mainWork != "" {
				if st, _, errText := s.partOutput(sess.mainWork); chatOver(st) {
					return fmt.Sprintf("the main part ended: %s", orState(errText, st))
				}
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				return "timed out"
			}
			select {
			case r := <-sess.stop:
				return r
			case <-time.After(time.Second):
			}
		}
		return ""
	}
	if why := wait(time.Now().Add(splitHelperWait), func() bool {
		for _, id := range sess.helperWork {
			if _, out, _ := s.partOutput(id); !strings.Contains(out, catalog.SplitHelperReady) {
				return false
			}
		}
		return true
	}); why != "" {
		end("while the helpers started: " + why)
		return
	}
	id, err := submit(sess.Main, capSplitMain, map[string]string{"session": sess.ID, "model": sess.Model, "name": sess.Name, "helpers": strconv.Itoa(len(sess.Helpers))})
	if err != nil {
		end(fmt.Sprintf("couldn't start the main part: %v", err))
		return
	}
	sess.mainWork = id
	s.setSplit(sess, SplitLoading, "")
	if why := wait(time.Time{}, func() bool {
		_, out, _ := s.partOutput(sess.mainWork)
		return strings.Contains(out, catalog.SplitMainReady)
	}); why != "" {
		end("while loading the model: " + why)
		return
	}
	s.setSplit(sess, SplitReady, "")
	s.splits.mu.Lock()
	sess.LastUsed = time.Now()
	s.splits.mu.Unlock()
	stopReason := ""
	why := wait(time.Time{}, func() bool {
		// The main device too: unplugged or in use, it would hold its
		// memory and keep-awake for nothing (chats to it get a 503).
		if rec, ok := s.Registry.Get(sess.Main); ok {
			if ok, reason := s.availableNow(rec, time.Now()); !ok {
				stopReason = fmt.Sprintf("the main device %s stopped taking work: %s", s.nodeDisplayName(sess.Main), reason)
				return true
			}
		}
		for _, h := range sess.Helpers {
			if rec, ok := s.Registry.Get(h); ok {
				if ok, reason := s.availableNow(rec, time.Now()); !ok {
					stopReason = fmt.Sprintf("helper %s stopped taking work: %s", s.nodeDisplayName(h), reason)
					return true
				}
			}
		}
		s.splits.mu.Lock()
		idle := time.Since(sess.LastUsed)
		s.splits.mu.Unlock()
		if idle > SplitIdleTimeout {
			stopReason = fmt.Sprintf("no chats for %s", SplitIdleTimeout)
			return true
		}
		return false
	})
	if why == "" {
		why = stopReason
	}
	end(why)
}

func orState(errText string, st domain.WorkloadState) string {
	if errText != "" {
		return errText
	}
	return strings.ToLower(string(st))
}

// attrValue is one capability attribute of rec.
func attrValue(rec *NodeRecord, capability domain.CapabilityName, attr string) string {
	for _, c := range rec.Capabilities {
		if c.Name == capability {
			return c.Attributes[attr]
		}
	}
	return ""
}

func (s *Server) splitView() any {
	s.splits.mu.Lock()
	defer s.splits.mu.Unlock()
	if s.splits.cur == nil {
		return map[string]any{"session": nil}
	}
	cp := *s.splits.cur
	return map[string]any{"session": cp}
}

// apiSplit: GET /llm/split (the session), POST (start), DELETE (stop).
func (s *Server) apiGetSplit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.splitView())
}

func (s *Server) apiStartSplit(w http.ResponseWriter, r *http.Request) {
	var req SplitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	sess, err := s.StartSplit(req)
	switch {
	case errors.Is(err, ErrSplitBusy):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, ErrInvalidWorkload):
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.audit(domain.AuditSecurity, "split.started", sess.Main, actorFrom(r.Context()), map[string]any{
		"session": sess.ID, "name": sess.Name, "helpers": sess.Helpers,
	})
	writeJSON(w, http.StatusAccepted, s.splitView())
}

func (s *Server) apiStopSplit(w http.ResponseWriter, r *http.Request) {
	if !s.StopSplit("stopped by the operator") {
		http.Error(w, "no split session", http.StatusNotFound)
		return
	}
	s.audit(domain.AuditSecurity, "split.stopped", "", actorFrom(r.Context()), nil)
	w.WriteHeader(http.StatusAccepted)
}
