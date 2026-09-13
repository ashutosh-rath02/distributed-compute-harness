package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func startTestServer(t *testing.T, idleTimeout time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := NewServer(idleTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return ln.Addr().String()
}

func TestListenAndConnectSpliceBytesBothDirections(t *testing.T) {
	addr := startTestServer(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	listenDone := make(chan net.Conn, 1)
	listenErr := make(chan error, 1)
	go func() {
		c, err := DialListen(ctx, addr, "session-a")
		if err != nil {
			listenErr <- err
			return
		}
		listenDone <- c
	}()

	// Give the listen side a moment to actually park at the relay before
	// connecting — otherwise this is racing DialConnect against DialListen
	// reaching the relay first, which is exactly the "no listener waiting"
	// case tested separately below.
	time.Sleep(50 * time.Millisecond)

	connConn, err := DialConnect(ctx, addr, "session-a")
	if err != nil {
		t.Fatalf("DialConnect: %v", err)
	}
	defer connConn.Close()

	var listenConn net.Conn
	select {
	case listenConn = <-listenDone:
	case err := <-listenErr:
		t.Fatalf("DialListen: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DialListen to pair")
	}
	defer listenConn.Close()

	if _, err := connConn.Write([]byte("hello-from-connect")); err != nil {
		t.Fatalf("connConn.Write: %v", err)
	}
	buf := make([]byte, len("hello-from-connect"))
	if _, err := io.ReadFull(listenConn, buf); err != nil {
		t.Fatalf("listenConn.Read: %v", err)
	}
	if string(buf) != "hello-from-connect" {
		t.Fatalf("got %q, want %q", buf, "hello-from-connect")
	}

	if _, err := listenConn.Write([]byte("hello-from-listen")); err != nil {
		t.Fatalf("listenConn.Write: %v", err)
	}
	buf = make([]byte, len("hello-from-listen"))
	if _, err := io.ReadFull(connConn, buf); err != nil {
		t.Fatalf("connConn.Read: %v", err)
	}
	if string(buf) != "hello-from-listen" {
		t.Fatalf("got %q, want %q", buf, "hello-from-listen")
	}
}

func TestConnectWithNoWaitingListenerFailsFast(t *testing.T) {
	addr := startTestServer(t, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := DialConnect(ctx, addr, "no-such-session")
	if err == nil {
		t.Fatal("expected DialConnect to fail with no listener waiting")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("DialConnect took %v to fail — expected a fast rejection, not a hang", elapsed)
	}
}

func TestIdleListenerTimesOutAndIsCleanedUp(t *testing.T) {
	addr := startTestServer(t, 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := DialListen(ctx, addr, "session-timeout")
	if err == nil {
		t.Fatal("expected DialListen to time out with no connect ever arriving")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("DialListen returned after %v, expected roughly the 100ms idle timeout", elapsed)
	}
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("expected errors.Is(err, ErrTimedOut), got %v", err)
	}
}

// TestTwoPendingListenersPairInFIFOOrder proves the relay doesn't cross-wire
// two concurrently-pending listeners for the same session — the manager
// side keeps a small pool of pending registrations open at once (v5 remote
// part 1's design), so this must not let a later connect claim an entry
// out of order in a way that mixes up which byte stream goes where.
func TestTwoPendingListenersPairInFIFOOrder(t *testing.T) {
	addr := startTestServer(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type result struct {
		conn net.Conn
		err  error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() {
		c, err := DialListen(ctx, addr, "session-fifo")
		first <- result{c, err}
	}()
	time.Sleep(30 * time.Millisecond)
	go func() {
		c, err := DialListen(ctx, addr, "session-fifo")
		second <- result{c, err}
	}()
	time.Sleep(30 * time.Millisecond)

	connConnA, err := DialConnect(ctx, addr, "session-fifo")
	if err != nil {
		t.Fatalf("first DialConnect: %v", err)
	}
	defer connConnA.Close()

	var firstListen net.Conn
	select {
	case r := <-first:
		if r.err != nil {
			t.Fatalf("first DialListen: %v", r.err)
		}
		firstListen = r.conn
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first DialListen to pair")
	}
	defer firstListen.Close()

	if _, err := connConnA.Write([]byte("A")); err != nil {
		t.Fatalf("write A: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(firstListen, buf); err != nil {
		t.Fatalf("read on first listener: %v", err)
	}
	if string(buf) != "A" {
		t.Fatalf("first listener got %q, want the connect that paired with it, not the second one", buf)
	}

	connConnB, err := DialConnect(ctx, addr, "session-fifo")
	if err != nil {
		t.Fatalf("second DialConnect: %v", err)
	}
	defer connConnB.Close()

	var secondListen net.Conn
	select {
	case r := <-second:
		if r.err != nil {
			t.Fatalf("second DialListen: %v", r.err)
		}
		secondListen = r.conn
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second DialListen to pair")
	}
	defer secondListen.Close()

	if _, err := connConnB.Write([]byte("B")); err != nil {
		t.Fatalf("write B: %v", err)
	}
	if _, err := io.ReadFull(secondListen, buf); err != nil {
		t.Fatalf("read on second listener: %v", err)
	}
	if string(buf) != "B" {
		t.Fatalf("second listener got %q, want %q", buf, "B")
	}
}
