package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
	"home-harness/internal/tunnel"
)

// Tunnels (internal/tunnel): the manager splices two devices' WebSockets
// so one device (the "main" side) can reach a service that another (the
// "helper") runs on its loopback only. A tunnel pair joins one main and
// one helper within a session and has a token per side, issued to that
// side's device alone (in its assignment, never persisted or shown) and
// valid until the session ends. Each TCP connection the main side makes
// becomes one spliced pair: the main side's WebSocket arrives with its
// token, the manager asks the helper's agent (TUNNEL_OPEN) to connect its
// local service and come back with its own token and the connection's
// id, and joins the two.

// tunnelWait is how long a main side waits for its helper to come back.
const tunnelWait = 15 * time.Second

type tunnelSide string

const (
	tunnelMain   tunnelSide = "main"
	tunnelHelper tunnelSide = "helper"
)

type tunnelPair struct {
	session      string
	index        int // which helper of the session
	main, helper domain.NodeID
}

type tunnelGrant struct {
	pair *tunnelPair
	side tunnelSide
}

// helperArrival is a helper side delivered to the main side waiting for
// it; done is closed when their splice ends.
type helperArrival struct {
	conn net.Conn
	done chan struct{}
}

type pendingTunnel struct {
	pair *tunnelPair
	ch   chan helperArrival
}

type tunnelTable struct {
	mu      sync.Mutex
	grants  map[[32]byte]tunnelGrant
	pending map[string]pendingTunnel // connection id -> the main side waiting
}

func newTunnelTable() *tunnelTable {
	return &tunnelTable{grants: map[[32]byte]tunnelGrant{}, pending: map[string]pendingTunnel{}}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// OpenTunnelPair issues the two tokens of a new tunnel pair: one for the
// main device, one for the helper.
func (s *Server) OpenTunnelPair(session string, index int, main, helper domain.NodeID) (mainToken, helperToken string, err error) {
	if mainToken, err = randomHex(32); err != nil {
		return "", "", err
	}
	if helperToken, err = randomHex(32); err != nil {
		return "", "", err
	}
	p := &tunnelPair{session: session, index: index, main: main, helper: helper}
	t := s.tunnels
	t.mu.Lock()
	t.grants[sha256.Sum256([]byte(mainToken))] = tunnelGrant{p, tunnelMain}
	t.grants[sha256.Sum256([]byte(helperToken))] = tunnelGrant{p, tunnelHelper}
	t.mu.Unlock()
	return mainToken, helperToken, nil
}

// CloseTunnelSession revokes every token of session. Connections already
// spliced end when either device stops its side.
func (s *Server) CloseTunnelSession(session string) {
	t := s.tunnels
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, g := range t.grants {
		if g.pair.session == session {
			delete(t.grants, k)
		}
	}
}

func (t *tunnelTable) lookup(token string) (tunnelGrant, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	g, ok := t.grants[sha256.Sum256([]byte(token))]
	return g, ok
}

// TunnelHandler serves /tunnel/{token} on the agent-facing listener.
func (s *Server) TunnelHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/tunnel/")
		g, ok := s.tunnels.lookup(token)
		if !ok {
			http.Error(w, "unknown or ended tunnel", http.StatusNotFound)
			return
		}
		switch g.side {
		case tunnelMain:
			s.serveTunnelMain(w, r, g.pair)
		case tunnelHelper:
			s.serveTunnelHelper(w, r, g.pair)
		}
	}
}

func (s *Server) serveTunnelMain(w http.ResponseWriter, r *http.Request, p *tunnelPair) {
	rec, ok := s.Registry.Get(p.helper)
	if !ok || rec.Conn == nil || rec.State != domain.NodeReady {
		http.Error(w, "the helper device isn't connected", http.StatusServiceUnavailable)
		return
	}
	connID, err := randomHex(16)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ch := make(chan helperArrival, 1)
	s.tunnels.mu.Lock()
	s.tunnels.pending[connID] = pendingTunnel{pair: p, ch: ch}
	s.tunnels.mu.Unlock()
	defer func() {
		s.tunnels.mu.Lock()
		delete(s.tunnels.pending, connID)
		s.tunnels.mu.Unlock()
	}()
	conn, err := tunnel.Accept(w, r)
	if err != nil {
		return
	}
	s.send(context.Background(), rec.Conn, protocol.MsgTunnelOpen, domain.ManagerNodeID, p.helper,
		protocol.TunnelOpenPayload{Session: p.session, Index: p.index, ConnID: connID})
	select {
	case h := <-ch:
		tunnel.Splice(conn, h.conn)
		close(h.done)
	case <-time.After(tunnelWait):
		conn.Close()
	}
}

func (s *Server) serveTunnelHelper(w http.ResponseWriter, r *http.Request, p *tunnelPair) {
	connID := r.URL.Query().Get("conn")
	s.tunnels.mu.Lock()
	pt, ok := s.tunnels.pending[connID]
	if ok && pt.pair == p {
		delete(s.tunnels.pending, connID) // one helper side per connection
	}
	s.tunnels.mu.Unlock()
	if !ok || pt.pair != p {
		http.Error(w, "no such tunnel connection", http.StatusNotFound)
		return
	}
	conn, err := tunnel.Accept(w, r)
	if err != nil {
		return
	}
	done := make(chan struct{})
	pt.ch <- helperArrival{conn, done}
	<-done
}
