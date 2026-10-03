package manager

import (
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// Join requests: devices asking to be admitted by the operator's approval
// (agent -pair) instead of a token. Nothing is admitted without someone
// tapping Approve after comparing the pairing code on both screens, so a
// request is safe to accept from anyone on the network — the limits below
// only keep a flood of them from crowding out the real one.
//
// A request lives in memory while its device keeps asking (the agent
// retries every couple of seconds): approving it admits the device at its
// next REGISTER. A manager restart forgets pending requests; the device
// simply asks again.
const (
	maxJoinRequests        = 16
	maxJoinRequestsPerHost = 3
	joinRequestIdle        = 30 * time.Second // the device stopped asking
	joinRequestMaxAge      = 15 * time.Minute
	joinApprovedHold       = 10 * time.Minute // approved, waiting for the device's next REGISTER
	joinRejectHold         = 10 * time.Minute
)

// JoinRequest is one device waiting for approval. Name and Hostname are
// whatever the device claims (clipped; render escaped).
type JoinRequest struct {
	NodeID      domain.NodeID   `json:"nodeId"`
	Name        string          `json:"name"`
	Hostname    string          `json:"hostname,omitempty"`
	Platform    domain.Platform `json:"platform"`
	Remote      string          `json:"remote"`
	Code        string          `json:"code"`
	RequestedAt time.Time       `json:"requestedAt"`
	Approved    bool            `json:"approved,omitempty"`

	lastSeen   time.Time
	approvedAt time.Time
}

type joinDecision int

const (
	joinPending joinDecision = iota
	joinApproved
	joinRejected
	joinFull
)

type joinRequests struct {
	mu       sync.Mutex
	byID     map[domain.NodeID]*JoinRequest
	rejected map[domain.NodeID]time.Time
	// expired collects requests dropped for going stale, whichever call
	// noticed, until prune reports them (the dashboard drops their cards).
	expired []JoinRequest
	now     func() time.Time
}

func newJoinRequests() *joinRequests {
	return &joinRequests{byID: map[domain.NodeID]*JoinRequest{}, rejected: map[domain.NodeID]time.Time{}, now: time.Now}
}

// request records (or refreshes) r's request and decides this REGISTER:
// approved (admit now), pending (tell the device to keep waiting),
// rejected, or full. isNew reports a request seen for the first time.
func (j *joinRequests) request(r JoinRequest) (d joinDecision, isNew bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	j.pruneLocked(now)
	if until, ok := j.rejected[r.NodeID]; ok && now.Before(until) {
		return joinRejected, false
	}
	if cur, ok := j.byID[r.NodeID]; ok {
		cur.lastSeen = now
		if cur.Approved {
			delete(j.byID, r.NodeID)
			return joinApproved, false
		}
		return joinPending, false
	}
	if len(j.byID) >= maxJoinRequests || j.fromHostLocked(r.Remote) >= maxJoinRequestsPerHost {
		return joinFull, false
	}
	r.RequestedAt, r.lastSeen, r.Approved = now, now, false
	j.byID[r.NodeID] = &r
	return joinPending, true
}

func (j *joinRequests) fromHostLocked(host string) int {
	n := 0
	for _, r := range j.byID {
		if r.Remote == host {
			n++
		}
	}
	return n
}

// approve marks id's pending request approved; the device is admitted at
// its next REGISTER.
func (j *joinRequests) approve(id domain.NodeID) (JoinRequest, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	j.pruneLocked(now)
	r, ok := j.byID[id]
	if !ok || r.Approved {
		return JoinRequest{}, false
	}
	r.Approved, r.approvedAt = true, now
	return *r, true
}

// reject drops id's request and refuses that device for a while (it would
// otherwise just ask again within seconds).
func (j *joinRequests) reject(id domain.NodeID) (JoinRequest, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	j.pruneLocked(now)
	r, ok := j.byID[id]
	if !ok {
		return JoinRequest{}, false
	}
	delete(j.byID, id)
	j.rejected[id] = now.Add(joinRejectHold)
	return *r, true
}

// list returns the current requests, oldest first.
func (j *joinRequests) list() []JoinRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pruneLocked(j.now())
	out := make([]JoinRequest, 0, len(j.byID))
	for _, r := range j.byID {
		out = append(out, *r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].RequestedAt.Before(out[b].RequestedAt) })
	return out
}

// prune drops requests whose device stopped asking or that waited too
// long, and returns every request dropped that way since the last call
// (for the dashboard to drop them too).
func (j *joinRequests) prune() []JoinRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pruneLocked(j.now())
	gone := j.expired
	j.expired = nil
	return gone
}

func (j *joinRequests) pruneLocked(now time.Time) {
	for id, r := range j.byID {
		stale := now.Sub(r.lastSeen) > joinRequestIdle || now.Sub(r.RequestedAt) > joinRequestMaxAge
		if r.Approved {
			stale = now.Sub(r.approvedAt) > joinApprovedHold
		}
		if stale {
			if len(j.expired) < 4*maxJoinRequests { // only a notice; never unbounded
				j.expired = append(j.expired, *r)
			}
			delete(j.byID, id)
		}
	}
	for id, until := range j.rejected {
		if !now.Before(until) {
			delete(j.rejected, id)
		}
	}
}

// remoteHost is the host part of a peer address ("1.2.3.4:5555" ->
// "1.2.3.4"): reconnects come from new ports, so limits go by host.
func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// ---- operator API

// apiListJoinRequests serves GET /join-requests.
func (s *Server) apiListJoinRequests(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.joinReqs.list())
}

// apiDecideJoinRequest serves POST /join-requests/{id}/approve and
// /reject. Approving admits the device at its next REGISTER (within a
// couple of seconds); the operator should have compared the codes.
func (s *Server) apiDecideJoinRequest(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := domain.NodeID(r.PathValue("id"))
		decide, kind, decision := s.joinReqs.reject, "node.join-rejected", "rejected"
		if approve {
			decide, kind, decision = s.joinReqs.approve, "node.join-approved", "approved"
		}
		req, ok := decide(id)
		if !ok {
			http.Error(w, "no such device waiting for approval (it may have stopped asking, or was already decided)", http.StatusNotFound)
			return
		}
		s.audit(domain.AuditSecurity, kind, id, actorFrom(r.Context()), map[string]any{
			"name": req.Name, "code": req.Code, "remote": req.Remote,
		})
		s.publish(domain.EventJoinDecided, id, map[string]any{"decision": decision})
		writeJSON(w, http.StatusOK, map[string]string{"nodeId": string(id), "decision": decision})
	}
}
