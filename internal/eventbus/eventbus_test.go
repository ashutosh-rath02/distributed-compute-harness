package eventbus

import (
	"testing"
	"time"

	"home-harness/internal/domain"
)

func TestSubscribeReceivesPublishedEvent(t *testing.T) {
	b := New()
	ch, unsubscribe := b.Subscribe(4)
	defer unsubscribe()

	b.Publish(domain.Event{Type: domain.EventNodeReady, NodeID: "node-a"})

	select {
	case e := <-ch:
		if e.Type != domain.EventNodeReady || e.NodeID != "node-a" {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published event")
	}
}

func TestPublishFansOutToAllSubscribers(t *testing.T) {
	b := New()
	ch1, unsub1 := b.Subscribe(4)
	defer unsub1()
	ch2, unsub2 := b.Subscribe(4)
	defer unsub2()

	b.Publish(domain.Event{Type: domain.EventNodeOffline, NodeID: "node-a"})

	for _, ch := range []<-chan domain.Event{ch1, ch2} {
		select {
		case e := <-ch:
			if e.Type != domain.EventNodeOffline {
				t.Fatalf("unexpected event: %+v", e)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for fanned-out event")
		}
	}
}

func TestPublishDoesNotBlockOnFullSubscriber(t *testing.T) {
	b := New()
	ch, unsubscribe := b.Subscribe(1) // tiny buffer, deliberately never drained
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(domain.Event{Type: domain.EventNodeUpdated})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
	<-ch // drain one, just to use the channel
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	b := New()
	ch, unsubscribe := b.Subscribe(1)
	unsubscribe()

	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}
}

func TestUnsubscribedListenerReceivesNothingFurther(t *testing.T) {
	b := New()
	ch, unsubscribe := b.Subscribe(4)
	unsubscribe()

	b.Publish(domain.Event{Type: domain.EventNodeReady})

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected no further events after unsubscribe")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected closed channel to be immediately readable (zero value, ok=false)")
	}
}
