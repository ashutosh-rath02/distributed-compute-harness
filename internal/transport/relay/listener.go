package relay

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"time"

	relayproto "home-harness/internal/relay"
)

// pendingPoolSize is how many "listen" registrations the manager side keeps
// parked at the relay concurrently. With only one slot, the instant a
// pairing consumes it there is a real window — big enough for a
// reconnecting agent to lose the race — before a replacement registration
// reaches the relay. A small pool keeps that window closed in practice
// without needing the relay's wire protocol to support multiplexing.
const pendingPoolSize = 3

// listener implements net.Listener by keeping pendingPoolSize concurrent
// relay.DialListen registrations open at all times, handing out whichever
// pairs first and immediately starting a replacement to keep the pool
// full. ws.Transport.ListenOn only ever calls Accept in a loop and never
// inspects Addr() meaningfully, so both are implemented minimally.
type listener struct {
	relayAddr string
	session   string

	ctx    context.Context
	cancel context.CancelFunc

	accepted chan net.Conn

	wg sync.WaitGroup
}

func newListener(relayAddr, session string) *listener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &listener{
		relayAddr: relayAddr,
		session:   session,
		ctx:       ctx,
		cancel:    cancel,
		accepted:  make(chan net.Conn),
	}
	for i := 0; i < pendingPoolSize; i++ {
		l.wg.Add(1)
		go l.keepDialing()
	}
	return l
}

// keepDialing repeatedly calls relay.DialListen, forwarding each success to
// accepted and immediately re-dialing to keep this pool slot occupied.
// relayproto.ErrTimedOut (the relay's own idle timeout expiring with
// nothing to pair) is the pool's steady-state refresh cycle under
// completely normal operation — every parked slot hits it repeatedly
// whenever no one's connecting, which on a healthy home manager is most of
// the time — so it re-dials immediately and silently. Any other error
// (relay unreachable, connection refused) is logged and retried after a
// short pause, since those genuinely indicate something worth a home
// operator's attention.
func (l *listener) keepDialing() {
	defer l.wg.Done()
	for {
		if l.ctx.Err() != nil {
			return
		}
		conn, err := relayproto.DialListen(l.ctx, l.relayAddr, l.session)
		if err != nil {
			if l.ctx.Err() != nil {
				return
			}
			if errors.Is(err, relayproto.ErrTimedOut) {
				continue
			}
			log.Printf("relay: listen registration failed, retrying: %v", err)
			select {
			case <-time.After(time.Second):
			case <-l.ctx.Done():
				return
			}
			continue
		}
		select {
		case l.accepted <- conn:
		case <-l.ctx.Done():
			conn.Close()
			return
		}
	}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accepted:
		return c, nil
	case <-l.ctx.Done():
		return nil, errors.New("relay: listener closed")
	}
}

// Close stops all pending relay registrations and waits for their
// goroutines to actually exit — not just signal cancellation — so that a
// caller which has called Close (e.g. a test tearing down) never observes
// this listener's background activity (retries, log lines) after Close
// returns.
func (l *listener) Close() error {
	l.cancel()
	l.wg.Wait()
	return nil
}

func (l *listener) Addr() net.Addr { return relayListenerAddr(l.relayAddr) }

type relayListenerAddr string

func (a relayListenerAddr) Network() string { return "relay" }
func (a relayListenerAddr) String() string  { return string(a) }
