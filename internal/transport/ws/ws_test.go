package ws

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/mtls"
)

// dialWithRetry tolerates Listen's HTTP server starting to accept
// connections asynchronously relative to when Listen returns.
func dialWithRetry(t *testing.T, client *Transport, addr string) domain.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		dialCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		c, err := client.Dial(dialCtx, addr)
		cancel()
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dial: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertRoundTrip(t *testing.T, server, client *Transport, addr string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conns, err := server.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	clientConn := dialWithRetry(t, client, addr)
	defer clientConn.Close()

	var serverConn domain.Conn
	select {
	case serverConn = <-conns:
	case <-time.After(2 * time.Second):
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
	assertRoundTrip(t, New(), New(), "127.0.0.1:18181")
}

func TestTLSRoundTrip(t *testing.T) {
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	server := NewTLSServer(cert)
	client := NewTLSClient(mtls.PinnedClientConfig(mtls.Fingerprint(cert)))
	assertRoundTrip(t, server, client, "127.0.0.1:18182")
}

func TestTLSDialRejectsMismatchedFingerprint(t *testing.T) {
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	const addr = "127.0.0.1:18183"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := NewTLSServer(cert)
	if _, err := server.Listen(ctx, addr); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// First prove the listener is actually up and reachable, with the
	// correct fingerprint — this rules out "connection refused because
	// Listen hasn't started accepting yet" as a false-positive reason for
	// the mismatched-fingerprint dial below to fail.
	goodConn := dialWithRetry(t, NewTLSClient(mtls.PinnedClientConfig(mtls.Fingerprint(cert))), addr)
	goodConn.Close()

	// Now the listener is confirmed reachable, so a single dial attempt
	// with the wrong fingerprint failing can only be the pinning check.
	client := NewTLSClient(mtls.PinnedClientConfig("0000000000000000000000000000000000000000000000000000000000000000"))
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	if _, err := client.Dial(dialCtx, addr); err == nil {
		t.Fatal("expected Dial to fail with a mismatched fingerprint")
	}
}
