package relay

import (
	"context"
	"net"
	"testing"
	"time"

	relayproto "home-harness/internal/relay"
)

// startShortIdleTestRelay is like startTestRelay but with a much shorter
// idle timeout, so a pool registration cycles through several
// relayproto.ErrTimedOut rounds within an ordinary test's lifetime.
func startShortIdleTestRelay(t *testing.T, idleTimeout time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := relayproto.NewServer(idleTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return ln.Addr().String()
}

// TestListenerStaysResponsiveAcrossIdleTimeoutCycles proves keepDialing's
// relayproto.ErrTimedOut handling (an expected, silent re-register) doesn't
// fall through to the generic error path's logged 1-second backoff pause —
// if it did, a connect arriving shortly after a timeout cycle could wait up
// to a second for the pool to notice it should re-register, which would
// reintroduce the reconnect-race window pendingPoolSize exists to close.
func TestListenerStaysResponsiveAcrossIdleTimeoutCycles(t *testing.T) {
	const idleTimeout = 80 * time.Millisecond
	relayAddr := startShortIdleTestRelay(t, idleTimeout)

	lis := newListener(relayAddr, "session-cycling")
	defer lis.Close()

	// Let several idle-timeout cycles elapse with nobody connecting —
	// this is exactly the steady-state condition #1 in the advisor review
	// was about: a healthy, idle manager cycling its pool continuously.
	time.Sleep(idleTimeout * 4)

	ctx, cancel := context.WithTimeout(context.Background(), idleTimeout*2)
	defer cancel()
	conn, err := relayproto.DialConnect(ctx, relayAddr, "session-cycling")
	if err != nil {
		t.Fatalf("DialConnect after idle cycling: %v", err)
	}
	defer conn.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := lis.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(idleTimeout * 2):
		t.Fatal("timed out waiting for the pool to accept a connection shortly after idle cycling — the pool likely fell into the generic error backoff instead of silently re-registering")
	}
}
