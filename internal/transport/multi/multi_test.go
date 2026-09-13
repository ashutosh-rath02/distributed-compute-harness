package multi

import (
	"context"
	"errors"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// fakeConn is the minimal domain.Conn needed to prove a connection made it
// through the fan-in — its content is never inspected.
type fakeConn struct{ tag string }

func (f *fakeConn) Send(context.Context, []byte) error      { return nil }
func (f *fakeConn) Receive(context.Context) ([]byte, error) { return nil, nil }
func (f *fakeConn) RemoteAddr() string                      { return f.tag }
func (f *fakeConn) Close() error                            { return nil }

// fakeTransport hands out exactly the conns given to it on Listen, then
// closes its channel once ctx is done.
type fakeTransport struct {
	conns []domain.Conn
}

func (f *fakeTransport) Dial(context.Context, string) (domain.Conn, error) {
	return nil, errors.New("fakeTransport: Dial not implemented")
}

func (f *fakeTransport) Listen(ctx context.Context, _ string) (<-chan domain.Conn, error) {
	ch := make(chan domain.Conn, len(f.conns))
	for _, c := range f.conns {
		ch <- c
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func TestListenFansInFromEveryAddedTransport(t *testing.T) {
	a := &fakeTransport{conns: []domain.Conn{&fakeConn{tag: "a1"}, &fakeConn{tag: "a2"}}}
	b := &fakeTransport{conns: []domain.Conn{&fakeConn{tag: "b1"}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mt := New().Add(a, "addr-a").Add(b, "addr-b")
	out, err := mt.Listen(ctx, "unused")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case c := <-out:
			got[c.RemoteAddr()] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for connection %d", i+1)
		}
	}
	for _, want := range []string{"a1", "a2", "b1"} {
		if !got[want] {
			t.Fatalf("expected to receive a connection tagged %q, got %v", want, got)
		}
	}
}

func TestListenWithNoTargetsFails(t *testing.T) {
	if _, err := New().Listen(context.Background(), "unused"); err == nil {
		t.Fatal("expected Listen with no added transports to fail")
	}
}

func TestDialIsNotSupported(t *testing.T) {
	if _, err := New().Dial(context.Background(), "anything"); err == nil {
		t.Fatal("expected Dial to fail on a composite transport")
	}
}

func TestOutChannelClosesWhenContextCanceled(t *testing.T) {
	a := &fakeTransport{}
	ctx, cancel := context.WithCancel(context.Background())

	out, err := New().Add(a, "addr-a").Listen(ctx, "unused")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected out to be closed, got a value instead")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for out to close after ctx cancellation")
	}
}
