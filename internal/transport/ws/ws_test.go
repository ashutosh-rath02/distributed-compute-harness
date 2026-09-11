package ws

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func TestRoundTrip(t *testing.T) {
	const addr = "127.0.0.1:18181"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := New()
	conns, err := srv.Listen(ctx, addr)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	client := New()
	var clientConn domain.Conn
	deadline := time.Now().Add(2 * time.Second)
	for {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		c, dialErr := client.Dial(dialCtx, addr)
		dialCancel()
		if dialErr == nil {
			clientConn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dial: %v", dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
