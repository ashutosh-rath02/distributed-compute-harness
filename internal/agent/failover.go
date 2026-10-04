package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

// Following a standby manager (roadmap item 17, domain.FeatureFailover).
// The active manager tells the agent its failover term (with a proof
// signed by the manager key) and where the pair's managers listen; the
// agent keeps both in its identity directory, per pinned manager identity,
// and:
//   - reports the term whenever it registers, so a manager that has been
//     superseded steps down instead of admitting it;
//   - refuses a manager whose term is lower than the one it has seen;
//   - on losing its manager, tries its configured (or discovered) address
//     first, then the others it was told about.

// managersFile, in the identity directory, keeps what the agent was told.
const managersFile = "managers.json"

// failoverDialTimeout bounds one candidate's dial when there are others to
// try: a powered-off manager never refuses, the dial just hangs.
const failoverDialTimeout = 5 * time.Second

type knownManagers struct {
	// Fingerprint is the manager identity this was learned from: a
	// different pin (re-paired, "Forget this manager") starts afresh.
	Fingerprint string   `json:"fingerprint"`
	Term        uint64   `json:"term,omitempty"`
	TermProof   []byte   `json:"termProof,omitempty"`
	Managers    []string `json:"managers,omitempty"`
}

type failoverMemory struct {
	mu     sync.Mutex
	known  knownManagers
	loaded bool
	// heard is when the manager last sent anything (unix nanoseconds).
	heard atomic.Int64
}

func (a *Agent) managersPath() string { return filepath.Join(a.cfg.IdentityDir, managersFile) }

// knownManagers is what this agent was told by the manager it pins now.
func (a *Agent) knownManagers() knownManagers {
	m := &a.failover
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		m.loaded = true
		if data, err := os.ReadFile(a.managersPath()); err == nil {
			if err := json.Unmarshal(data, &m.known); err != nil {
				log.Printf("agent %s: ignoring %s: %v", a.identity.NodeID, managersFile, err)
				m.known = knownManagers{}
			}
		}
	}
	if m.known.Fingerprint != a.managerFingerprint() {
		return knownManagers{}
	}
	k := m.known
	k.Managers = slices.Clone(k.Managers)
	return k
}

// remember keeps a term (only ever upward) and the pair's addresses.
func (a *Agent) remember(term uint64, proof []byte, managers []string) {
	fp := a.managerFingerprint()
	current := a.knownManagers()
	k := current
	k.Fingerprint = fp
	if term > k.Term || (term == k.Term && len(k.TermProof) == 0 && len(proof) > 0) {
		k.Term, k.TermProof = term, proof
	}
	if list := validManagers(managers); len(list) > 0 {
		k.Managers = list
	}
	if k.Fingerprint == current.Fingerprint && k.Term == current.Term && slices.Equal(k.Managers, current.Managers) && len(k.TermProof) == len(current.TermProof) {
		return
	}
	m := &a.failover
	m.mu.Lock()
	m.known = k
	m.mu.Unlock()
	data, _ := json.MarshalIndent(k, "", "  ")
	if err := os.WriteFile(a.managersPath(), data, 0o600); err != nil {
		log.Printf("agent %s: could not keep the manager addresses: %v", a.identity.NodeID, err)
		return
	}
	if !slices.Equal(k.Managers, current.Managers) {
		log.Printf("agent %s: managers to try: %v (failover term %d)", a.identity.NodeID, k.Managers, k.Term)
	}
}

// validManagers keeps at most 8 plausible host:port entries.
func validManagers(in []string) []string {
	var out []string
	for _, addr := range in {
		if host, port, err := net.SplitHostPort(addr); err != nil || host == "" || port == "" || len(addr) > 255 {
			continue
		}
		if !slices.Contains(out, addr) {
			out = append(out, addr)
		}
		if len(out) == 8 {
			break
		}
	}
	return out
}

// errBehind: the manager that answered has a lower term than this agent
// has seen, so it is a superseded one (or a build without failover).
var errBehind = errors.New("behind")

// noteAck checks a REGISTER_ACK's term and keeps what it says.
func (a *Agent) noteAck(addr string, ack protocol.RegisterAckPayload) error {
	if k := a.knownManagers(); ack.Term < k.Term {
		return fmt.Errorf("the manager at %s is %w: failover term %d, but this device has seen %d (a standby took over): not using it", addr, errBehind, ack.Term, k.Term)
	}
	a.remember(ack.Term, ack.TermProof, ack.Managers)
	return nil
}

func (a *Agent) handleManagers(env *protocol.Envelope) {
	var p protocol.ManagersPayload
	if err := env.DecodePayload(&p); err != nil {
		return
	}
	a.remember(p.Term, p.TermProof, p.Managers)
}

// managerCandidates is where to look for the manager, in order: the
// configured or discovered address, then (with failover) the pair's other
// addresses this agent was told about.
func (a *Agent) managerCandidates(resolved string) []string {
	var out []string
	if resolved != "" {
		out = append(out, resolved)
	}
	if !a.cfg.Failover {
		return out
	}
	for _, addr := range a.knownManagers().Managers {
		if !slices.Contains(out, addr) {
			out = append(out, addr)
		}
	}
	return out
}

// connectFirst connects to the first candidate that answers as the active
// manager and admits this agent.
func (a *Agent) connectFirst(ctx context.Context, addrs []string) (domain.Conn, string, error) {
	var lastErr error
	for i, addr := range addrs {
		conn, err := a.dialAndRegister(ctx, addr, len(addrs) > 1)
		if err == nil {
			return conn, addr, nil
		}
		if isPendingApproval(err) || isJoinClosed(err) || ctx.Err() != nil {
			return nil, "", err
		}
		lastErr = err
		if i < len(addrs)-1 {
			log.Printf("agent %s: %v; trying %s", a.identity.NodeID, err, addrs[i+1])
		}
	}
	return nil, "", lastErr
}

// watchManager notices a manager that went silent without closing the
// connection (its PC slept or lost power: no reset ever arrives, and the
// OS may take many minutes to give up). It pings every heartbeat — the
// manager answers PONG — and ends the connection once nothing at all has
// come back for three heartbeats (10 s at least).
func (a *Agent) watchManager(ctx context.Context, conn domain.Conn, errCh chan<- error) {
	limit := max(3*a.cfg.HeartbeatInterval, 10*time.Second)
	a.failover.heard.Store(time.Now().UnixNano())
	tick := time.NewTicker(a.cfg.HeartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if silent := time.Since(time.Unix(0, a.failover.heard.Load())); silent > limit {
			errCh <- fmt.Errorf("the manager stopped answering (nothing for %s)", silent.Round(time.Second))
			return
		}
		a.send(ctx, conn, protocol.MsgPing, domain.ManagerNodeID, nil)
	}
}
