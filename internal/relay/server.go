package relay

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// DefaultIdleTimeout is how long a parked "listen" registration waits for a
// matching "connect" before the relay gives up on it and closes it —
// bounds how many abandoned registrations (e.g. from a manager that
// crashed mid-registration) a session can accumulate.
const DefaultIdleTimeout = 60 * time.Second

// pendingEntry is one parked "listen" connection awaiting a peer.
type pendingEntry struct {
	conn    net.Conn
	claimed chan struct{}
}

// Server pairs "listen" and "connect" connections that share a session
// token and splices each pair into one bidirectional byte stream. It holds
// no state beyond currently-pending registrations — nothing here persists
// across a restart, matching cmd/relay's role as a small, disposable,
// always-on forwarder rather than a stateful service.
type Server struct {
	idleTimeout time.Duration

	mu      sync.Mutex
	pending map[string][]*pendingEntry
}

// NewServer returns a relay server. idleTimeout <= 0 uses DefaultIdleTimeout.
func NewServer(idleTimeout time.Duration) *Server {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	return &Server{idleTimeout: idleTimeout, pending: make(map[string][]*pendingEntry)}
}

// Serve accepts connections on ln until ctx is canceled or Accept fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	var hs handshake
	if err := readJSONLine(conn, &hs); err != nil {
		conn.Close()
		return
	}
	if hs.Session == "" {
		writeJSONLine(conn, pairedResponse{Error: "missing session token"})
		conn.Close()
		return
	}

	switch hs.Role {
	case roleListen:
		s.handleListen(hs.Session, conn)
	case roleConnect:
		s.handleConnect(hs.Session, conn)
	default:
		writeJSONLine(conn, pairedResponse{Error: "unknown role"})
		conn.Close()
	}
}

// park registers e as pending for session.
func (s *Server) park(session string, e *pendingEntry) {
	s.mu.Lock()
	s.pending[session] = append(s.pending[session], e)
	s.mu.Unlock()
}

// remove deletes e from session's pending list if it's still there,
// reporting whether it did — false means a concurrent claim already won.
func (s *Server) remove(session string, e *pendingEntry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.pending[session]
	for i, cand := range list {
		if cand == e {
			s.pending[session] = append(list[:i:i], list[i+1:]...)
			return true
		}
	}
	return false
}

// claim atomically pops the oldest pending entry for session (FIFO), or
// nil if none is waiting.
func (s *Server) claim(session string) *pendingEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.pending[session]
	if len(list) == 0 {
		return nil
	}
	e := list[0]
	s.pending[session] = list[1:]
	return e
}

func (s *Server) handleListen(session string, conn net.Conn) {
	e := &pendingEntry{conn: conn, claimed: make(chan struct{})}
	s.park(session, e)

	select {
	case <-e.claimed:
		// handleConnect claimed this entry and owns conn now (it already
		// wrote the paired response and is splicing) — nothing left to do.
		return
	case <-time.After(s.idleTimeout):
		// remove() is the tie-breaker: if a connect claimed e in the same
		// instant this timeout fired, remove() finds it already gone and
		// correctly reports false, so we never touch a conn handleConnect
		// now owns.
		if s.remove(session, e) {
			writeJSONLine(conn, pairedResponse{Error: timedOutErrorText})
			conn.Close()
		}
	}
}

func (s *Server) handleConnect(session string, conn net.Conn) {
	e := s.claim(session)
	if e == nil {
		writeJSONLine(conn, pairedResponse{Error: "no listener waiting for this session"})
		conn.Close()
		return
	}

	close(e.claimed)
	if err := writeJSONLine(e.conn, pairedResponse{Paired: true, PeerAddr: conn.RemoteAddr().String()}); err != nil {
		e.conn.Close()
		conn.Close()
		return
	}
	if err := writeJSONLine(conn, pairedResponse{Paired: true, PeerAddr: e.conn.RemoteAddr().String()}); err != nil {
		e.conn.Close()
		conn.Close()
		return
	}
	splice(e.conn, conn)
}

// splice copies bytes in both directions until either side closes or
// errors, then closes both — the relay's entire job once a pair forms.
// Copy errors are never logged: at this layer, every one of them is just
// what a normal disconnect (either peer closing, or one side detecting the
// other's closure mid-copy) looks like — there's no reliable, portable way
// to tell that apart from a genuine problem from inside io.Copy's error,
// and logging every ordinary hangup would drown out the (rare, actionable)
// listen-registration failures keepDialing does log.
func splice(a, b net.Conn) {
	defer a.Close()
	defer b.Close()

	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
}
