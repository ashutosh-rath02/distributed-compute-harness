// Package eventbus is the harness's internal pub/sub for domain.Event.
// It exists so a future scheduler, logger, or dashboard can observe
// lifecycle/command events without being coupled to networking code
// (v1.md §12).
package eventbus

import (
	"sync"

	"home-harness/internal/domain"
)

// Bus fans out published events to every current subscriber. A slow or
// stalled subscriber never blocks publishing or other subscribers — an
// event it can't keep up with is dropped for that subscriber only.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan domain.Event]struct{}
}

// New returns an empty event bus.
func New() *Bus {
	return &Bus{subs: make(map[chan domain.Event]struct{})}
}

// Publish delivers e to every current subscriber's buffered channel,
// non-blocking: a subscriber whose buffer is full misses this event
// rather than stalling the publisher.
func (b *Bus) Publish(e domain.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe registers a new listener with the given buffer size and
// returns its channel plus an unsubscribe function. The channel is closed
// when unsubscribe is called; callers must call it exactly once to avoid
// leaking the subscription.
func (b *Bus) Subscribe(buffer int) (<-chan domain.Event, func()) {
	ch := make(chan domain.Event, buffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, unsubscribe
}
