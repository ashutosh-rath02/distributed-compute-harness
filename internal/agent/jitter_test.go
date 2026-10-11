package agent

import (
	"testing"
	"time"
)

// A fleet that loses its manager together must not come back together:
// the delays spread across the window instead of landing on one value.
func TestReconnectJitterSpreadsWithinBounds(t *testing.T) {
	base := 3 * time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		d := jitterFn(base)
		if d < base/2 || d >= base+base/2 {
			t.Fatalf("delay %v outside [%v, %v)", d, base/2, base+base/2)
		}
		seen[d/(100*time.Millisecond)] = true
	}
	if len(seen) < 8 {
		t.Fatalf("500 delays fell into only %d 100ms buckets: not spread", len(seen))
	}
	if jitterFn(0) != 0 {
		t.Error("zero must stay zero")
	}
}
