package manager

import (
	"context"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"home-harness/internal/domain"
)

// The audit log is the durable record of security-relevant actions —
// who was admitted and how, who was refused, and what the operator did.
// It is an operator's record, not a defense against the operator: anyone
// holding the operator token can delete the database.
//
// Two independently capped logs, so peer-driven noise (rejections,
// reconnects) can never evict operator history: anyone who can reach the
// agent port can send REGISTERs with a fresh keypair each time, and a
// phone on flaky Wi-Fi reconnects all day.
const (
	auditSecurityKeep   = 5000
	auditAdmissionsKeep = 5000
	auditNoiseKeep      = 2000

	// Rejection limits: one entry per (identity or address, reason) per
	// window — bounded map, since fresh identities are free to mint —
	// plus a global per-minute ceiling summarised once when exceeded.
	rejectionWindow      = 10 * time.Minute
	rejectionKeysMax     = 1024
	rejectionsPerMinute  = 30
	auditMemoryFallbackN = 500 // per log, when running without a store (tests)
)

// Actors recorded on operator actions: which credential form acted.
const (
	actorOperatorToken    = "operator-token"
	actorDashboardSession = "dashboard-session"
	actorUnauthenticated  = "operator" // operator auth disabled (tests, embedding)
	actorNode             = "node"
	actorAnonymous        = "anonymous"
)

type actorKey struct{}

func withActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

func actorFrom(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok {
		return a
	}
	return actorUnauthenticated
}

type auditRecorder struct {
	mu         sync.Mutex
	lastByKey  map[string]time.Time
	minute     time.Time
	inMinute   int
	suppressed int
	memory     map[domain.AuditLog][]domain.AuditEntry
	memorySeq  map[domain.AuditLog]uint64
}

func newAuditRecorder() *auditRecorder {
	return &auditRecorder{
		lastByKey: make(map[string]time.Time),
		memory:    make(map[domain.AuditLog][]domain.AuditEntry),
		memorySeq: make(map[domain.AuditLog]uint64),
	}
}

// audit appends one entry. It never fails the caller's action: an
// entry that can't be written is logged and dropped.
func (s *Server) audit(logName domain.AuditLog, kind string, nodeID domain.NodeID, actor string, detail map[string]any) {
	// The security log is reserved for actions taken with the operator
	// credential; anything a peer triggers is rerouted rather than ever
	// competing with (and evicting) it.
	if logName == domain.AuditSecurity && (actor == actorNode || actor == actorAnonymous) {
		log.Printf("manager: BUG: peer-driven audit entry %q routed to the security log; recording it in admissions", kind)
		logName = domain.AuditAdmissions
	}
	e := domain.AuditEntry{Log: logName, Time: time.Now().UTC(), Kind: kind, NodeID: nodeID, Actor: actor, Detail: detail}
	keep := auditSecurityKeep
	switch logName {
	case domain.AuditNoise:
		keep = auditNoiseKeep
	case domain.AuditAdmissions:
		keep = auditAdmissionsKeep
	}
	if s.store != nil {
		if _, err := s.store.AppendAudit(e, keep); err != nil {
			log.Printf("manager: audit %s: %v", kind, err)
		}
		return
	}
	r := s.auditLog
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memorySeq[logName]++
	e.Seq = r.memorySeq[logName]
	entries := append(r.memory[logName], e)
	if len(entries) > auditMemoryFallbackN {
		entries = entries[len(entries)-auditMemoryFallbackN:]
	}
	r.memory[logName] = entries
}

// auditRejection records a refused registration (or sign-in) in the noise
// log, rate-limited per key and globally — see the constants above.
// nodeID must be a *verified* identity (derived from a key whose signature
// checked out) or empty: a NodeID a peer merely claims belongs in extra,
// never in the entry's NodeID, or anyone could make the log say a victim
// was refused.
func (s *Server) auditRejection(kind string, nodeID domain.NodeID, remote, reason string, extra map[string]any) {
	now := time.Now().UTC()
	r := s.auditLog
	r.mu.Lock()
	summary := r.rollMinuteLocked(now)
	// Keyed by identity when there is one, else by the peer's host — never
	// the full address: every reconnect arrives from a new ephemeral port,
	// so a port-keyed limit would never dedupe a retrying agent.
	who := string(nodeID)
	if who == "" {
		who = remote
		if host, _, err := net.SplitHostPort(remote); err == nil {
			who = host
		}
	}
	key := clip(who) + "|" + reason
	if last, seen := r.lastByKey[key]; seen && now.Sub(last) < rejectionWindow {
		r.mu.Unlock()
		s.writeSuppressedSummary(summary)
		return
	}
	if r.inMinute >= rejectionsPerMinute {
		r.suppressed++
		r.mu.Unlock()
		s.writeSuppressedSummary(summary)
		return
	}
	r.inMinute++
	if len(r.lastByKey) >= rejectionKeysMax {
		for k, t := range r.lastByKey {
			if now.Sub(t) >= rejectionWindow {
				delete(r.lastByKey, k)
			}
		}
	}
	if len(r.lastByKey) < rejectionKeysMax {
		r.lastByKey[key] = now
	}
	r.mu.Unlock()
	s.writeSuppressedSummary(summary)
	actor := actorAnonymous
	if nodeID != "" {
		actor = actorNode
	}
	detail := map[string]any{"reason": reason, "remote": remote}
	for k, v := range extra {
		detail[k] = v
	}
	s.audit(domain.AuditNoise, kind, nodeID, actor, detail)
}

// rollMinuteLocked starts a new rejection-ceiling minute when now has
// moved past the current one, returning the previous minute's summary
// (if anything was suppressed) for the caller to write after unlocking.
func (r *auditRecorder) rollMinuteLocked(now time.Time) map[string]any {
	minute := now.Truncate(time.Minute)
	if minute.Equal(r.minute) {
		return nil
	}
	var summary map[string]any
	if r.suppressed > 0 {
		summary = map[string]any{"suppressed": r.suppressed, "minute": r.minute.Format(time.RFC3339)}
	}
	r.minute, r.inMinute, r.suppressed = minute, 0, 0
	return summary
}

func (s *Server) writeSuppressedSummary(summary map[string]any) {
	if summary != nil {
		s.audit(domain.AuditNoise, "node.rejected-suppressed", "", actorAnonymous, summary)
	}
}

// flushSuppressed writes a pending summary for a minute that has ended
// with no later rejection to trigger it. Called from the heartbeat
// monitor's ticker, so reading the log (GET /audit) never writes.
func (s *Server) flushSuppressed() {
	r := s.auditLog
	r.mu.Lock()
	summary := r.rollMinuteLocked(time.Now().UTC())
	r.mu.Unlock()
	s.writeSuppressedSummary(summary)
}

func (s *Server) rejectionKeys() int {
	s.auditLog.mu.Lock()
	defer s.auditLog.mu.Unlock()
	return len(s.auditLog.lastByKey)
}

// AuditEntries returns up to limit entries of one log, newest first. An
// empty logName means the operator's default view: security and
// admissions merged by time.
func (s *Server) AuditEntries(logName domain.AuditLog, limit int) ([]domain.AuditEntry, error) {
	if logName == "" {
		security, err := s.AuditEntries(domain.AuditSecurity, limit)
		if err != nil {
			return nil, err
		}
		admissions, err := s.AuditEntries(domain.AuditAdmissions, limit)
		if err != nil {
			return nil, err
		}
		merged := append(security, admissions...)
		sort.SliceStable(merged, func(i, j int) bool { return merged[i].Time.After(merged[j].Time) })
		if limit > 0 && len(merged) > limit {
			merged = merged[:limit]
		}
		return merged, nil
	}
	if s.store != nil {
		return s.store.ListAudit(logName, limit)
	}
	r := s.auditLog
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.memory[logName]
	var out []domain.AuditEntry
	for i := len(entries) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, entries[i])
	}
	return out, nil
}

// apiListAudit serves GET /audit?log=&limit=N: log empty (default) for
// security + admissions merged, or one of security, admissions, noise.
func (s *Server) apiListAudit(w http.ResponseWriter, r *http.Request) {
	logName := domain.AuditLog(r.URL.Query().Get("log"))
	switch logName {
	case "", domain.AuditSecurity, domain.AuditAdmissions, domain.AuditNoise:
	default:
		http.Error(w, "log must be security, admissions, or noise (or omitted for security+admissions)", http.StatusBadRequest)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(n, 1000)
	}
	entries, err := s.AuditEntries(logName, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []domain.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// clip bounds a peer-supplied string before it reaches an audit entry or
// a limiter key; otherwise each could be as large as a WebSocket frame.
func clip(s string) string {
	const max = 128
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// tokenPrefix identifies a one-time token in the audit log without
// recording it (it may still be live when the entry is written).
func tokenPrefix(token string) string {
	if len(token) > 8 {
		return token[:8] + "…"
	}
	return "…"
}
