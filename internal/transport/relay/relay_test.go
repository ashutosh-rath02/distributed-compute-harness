package relay

import (
	"context"
	"net"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/mtls"
	relayproto "home-harness/internal/relay"
)

// startTestRelay runs an in-process relay server (internal/relay) and
// returns its address, mirroring how ws_test.go tests against a real
// listener rather than a mock.
func startTestRelay(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := relayproto.NewServer(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return ln.Addr().String()
}

// dialWithRetry tolerates the relay-backed Listen's pool of registrations
// reaching the relay server asynchronously relative to when Listen
// returns (listener.go's keepDialing goroutines need a real network round
// trip to park at the relay) — the same category of async-startup
// tolerance internal/transport/ws's own dialWithRetry provides for a plain
// net.Listen's HTTP server starting to accept.
//
// Deliberately no per-attempt sub-timeout (unlike ws_test.go's version):
// a relay Dial is two network round trips stacked (the relay handshake,
// then the WS upgrade over the resulting conn), and a relay "connect"
// pairing is a one-shot, non-idempotent claim on the manager's pool — if a
// short per-attempt deadline aborted a call after the relay had already
// paired it but before the WS upgrade finished, the retry loop would
// silently burn through pendingPoolSize slots on abandoned-but-succeeded
// pairings. Real callers (internal/agent's reconnect loop) don't impose a
// per-attempt deadline either, so this matches production behavior rather
// than a stricter test-only bound.
func dialWithRetry(t *testing.T, client *Transport, addr string) domain.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := client.Dial(context.Background(), addr)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dial: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertRoundTrip(t *testing.T, relayAddr string, server, client *Transport) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conns, err := server.Listen(ctx, relayAddr)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	clientConn := dialWithRetry(t, client, relayAddr)
	defer clientConn.Close()

	var serverConn domain.Conn
	select {
	case serverConn = <-conns:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server to accept connection")
	}
	defer serverConn.Close()

	if err := clientConn.Send(context.Background(), []byte("hello-manager")); err != nil {
		t.Fatalf("client Send: %v", err)
	}
	got, err := serverConn.Receive(context.Background())
	if err != nil {
		t.Fatalf("server Receive: %v", err)
	}
	if string(got) != "hello-manager" {
		t.Fatalf("expected %q, got %q", "hello-manager", got)
	}

	if err := serverConn.Send(context.Background(), []byte("hello-agent")); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	got, err = clientConn.Receive(context.Background())
	if err != nil {
		t.Fatalf("client Receive: %v", err)
	}
	if string(got) != "hello-agent" {
		t.Fatalf("expected %q, got %q", "hello-agent", got)
	}
}

func TestRoundTrip(t *testing.T) {
	relayAddr := startTestRelay(t)
	assertRoundTrip(t, relayAddr, New("session-plain"), NewClient("session-plain"))
}

func TestTLSRoundTrip(t *testing.T) {
	relayAddr := startTestRelay(t)
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	server := NewTLSServer("session-tls", cert)
	client := NewTLSClient("session-tls", mtls.PinnedClientConfig(mtls.Fingerprint(cert)))
	assertRoundTrip(t, relayAddr, server, client)
}

func TestTLSDialRejectsMismatchedFingerprint(t *testing.T) {
	relayAddr := startTestRelay(t)
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := NewTLSServer("session-mismatch", cert)
	if _, err := server.Listen(ctx, relayAddr); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	client := NewTLSClient("session-mismatch", mtls.PinnedClientConfig("0000000000000000000000000000000000000000000000000000000000000000"))
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dialCancel()
	if _, err := client.Dial(dialCtx, relayAddr); err == nil {
		t.Fatal("expected Dial to fail with a mismatched fingerprint")
	}
}

// TestReconnectImmediatelyAfterDropSucceeds proves the pool-based listener
// (listener.go, pendingPoolSize > 1) keeps enough spare registrations
// parked at the relay that an agent reconnecting right after a drop — the
// realistic case agents actually hit, per the agent's own exponential
// backoff starting small (internal/agent/agent.go) — doesn't lose the race
// against the manager replenishing the one slot the first connection used.
func TestReconnectImmediatelyAfterDropSucceeds(t *testing.T) {
	relayAddr := startTestRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := New("session-reconnect")
	conns, err := server.Listen(ctx, relayAddr)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	client := NewClient("session-reconnect")

	firstConn := dialWithRetry(t, client, relayAddr)
	var firstServerConn domain.Conn
	select {
	case firstServerConn = <-conns:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the first pairing")
	}
	// Fire-and-forget, not a blocking Close(): a graceful WS close
	// handshake waits for the peer to ack, and nothing here is running a
	// receive loop to send that ack — a real dropped connection (WiFi
	// blip, killed process) never gets a graceful close either, so this
	// is the more realistic simulation of "drop" as well as the one that
	// lets the test proceed immediately, which is the point — the pool
	// must already have another registration parked before the manager
	// gets a chance to replenish the one just consumed (the reconnect
	// race advisor flagged).
	go firstConn.Close()
	go firstServerConn.Close()

	secondConn, err := client.Dial(ctx, relayAddr)
	if err != nil {
		t.Fatalf("Dial immediately after drop: %v", err)
	}
	defer secondConn.Close()

	select {
	case secondServerConn := <-conns:
		defer secondServerConn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the reconnect pairing")
	}
}
