package relay

import (
	"context"
	"crypto/cipher"
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

// connectGrace is how long a "connect" that finds no parked "listen" waits
// for one to arrive before failing. A manager's pool of listen slots
// (internal/transport/relay) re-registers the instant this relay's idle
// timeout expires them — and since the slots registered together, they
// expire together — so every idle cycle has a window of one
// manager<->relay round trip with nothing parked. Without a grace period, a
// connect landing in that window fails even though the manager is healthy,
// and the agent then sits out a reconnect backoff for nothing. It stays
// well under a second so a connect for a session with no manager at all
// still fails fast.
const connectGrace = 500 * time.Millisecond

// pendingEntry is one parked "listen" connection awaiting a peer.
type pendingEntry struct {
	conn    net.Conn
	claimed chan struct{}
}

// arrival lets connects waiting out connectGrace for a session learn that
// a listen has parked: park closes ch. waiters counts the connects
// currently holding it, so the last one to give up can drop the map entry
// — otherwise connects for never-listened sessions would leak entries.
type arrival struct {
	ch      chan struct{}
	waiters int
}

// Server pairs "listen" and "connect" connections that share a session
// token and splices each pair into one bidirectional byte stream. It holds
// no state beyond currently-pending registrations — nothing here persists
// across a restart, matching cmd/relay's role as a small, disposable,
// always-on forwarder rather than a stateful service.
type Server struct {
	idleTimeout time.Duration

	mu          sync.Mutex
	pending     map[string][]*pendingEntry
	arrivals    map[string]*arrival
	aliasCipher cipher.AEAD
}

// NewServer returns a relay server. idleTimeout <= 0 uses DefaultIdleTimeout.
func NewServer(idleTimeout time.Duration) *Server {
	return NewServerWithAliasKey(idleTimeout, "")
}

// NewServerWithAliasKey enables restart-stable, opaque per-device session
// aliases used by public enrollment. The key must remain stable across
// relay restarts or previously enrolled devices cannot resolve their alias.
func NewServerWithAliasKey(idleTimeout time.Duration, aliasKey string) *Server {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	aead, err := newAliasCipher(aliasKey)
	if err != nil {
		panic(err) // AES-GCM construction with a SHA-256 key cannot fail
	}
	return &Server{
		idleTimeout: idleTimeout,
		pending:     make(map[string][]*pendingEntry),
		arrivals:    make(map[string]*arrival),
		aliasCipher: aead,
	}
}

func (s *Server) issueAlias(session string) (string, error) { return sealAlias(s.aliasCipher, session) }

func (s *Server) resolveSession(session string) string {
	if resolved, ok := openAlias(s.aliasCipher, session); ok {
		return resolved
	}
	return session
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

// park registers e as pending for session and wakes any connects waiting
// for one.
func (s *Server) park(session string, e *pendingEntry) {
	s.mu.Lock()
	s.pending[session] = append(s.pending[session], e)
	if a := s.arrivals[session]; a != nil {
		close(a.ch)
		delete(s.arrivals, session)
	}
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

// claimOrWait atomically pops the oldest pending entry for session (FIFO),
// or, if none is waiting, registers the caller as waiting for the next
// park and returns that arrival instead.
func (s *Server) claimOrWait(session string) (*pendingEntry, *arrival) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if list := s.pending[session]; len(list) > 0 {
		e := list[0]
		s.pending[session] = list[1:]
		return e, nil
	}
	a := s.arrivals[session]
	if a == nil {
		a = &arrival{ch: make(chan struct{})}
		s.arrivals[session] = a
	}
	a.waiters++
	return nil, a
}

// stopWaiting releases a claimOrWait registration, dropping the arrival
// once its last waiter leaves (unless park already replaced/removed it).
func (s *Server) stopWaiting(session string, a *arrival) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.waiters--
	if a.waiters == 0 && s.arrivals[session] == a {
		delete(s.arrivals, session)
	}
}

// claimWithin claims a pending entry for session, waiting up to grace for
// one to park if none is there yet. A wake-up can lose the race for the
// new entry to another waiting connect, so it loops until the deadline.
func (s *Server) claimWithin(session string, grace time.Duration) *pendingEntry {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		e, a := s.claimOrWait(session)
		if e != nil {
			return e
		}
		select {
		case <-a.ch:
			s.stopWaiting(session, a)
		case <-timer.C:
			s.stopWaiting(session, a)
			return nil
		}
	}
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
	session = s.resolveSession(session)
	e := s.claimWithin(session, connectGrace)
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
