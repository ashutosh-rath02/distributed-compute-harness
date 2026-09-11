package agent

import (
	"testing"
	"time"
)

func TestNextBackoffDoubles(t *testing.T) {
	got := nextBackoff(1*time.Second, 30*time.Second)
	if got != 2*time.Second {
		t.Fatalf("expected 2s, got %s", got)
	}
}

func TestNextBackoffCapsAtMax(t *testing.T) {
	got := nextBackoff(20*time.Second, 30*time.Second)
	if got != 30*time.Second {
		t.Fatalf("expected capped at 30s, got %s", got)
	}
}

func TestNextBackoffStaysAtCapOnceReached(t *testing.T) {
	got := nextBackoff(30*time.Second, 30*time.Second)
	if got != 30*time.Second {
		t.Fatalf("expected to stay at 30s cap, got %s", got)
	}
}

func TestNextBackoffProgression(t *testing.T) {
	backoff := 1 * time.Second
	want := []time.Duration{2, 4, 8, 16, 30, 30}
	for i, w := range want {
		backoff = nextBackoff(backoff, 30*time.Second)
		wantDur := w * time.Second
		if backoff != wantDur {
			t.Fatalf("step %d: expected %s, got %s", i, wantDur, backoff)
		}
	}
}
