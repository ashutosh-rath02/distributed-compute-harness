package manager

import (
	"testing"
	"time"

	"home-harness/internal/domain"
)

// The backup route returns the TLS key and every token: no remote mode,
// not even full control, may reach it (or the health view).
func TestBackupIsNeverAvailableRemotely(t *testing.T) {
	for _, pattern := range []string{"POST /backup", "GET /reliability"} {
		for _, mode := range []RemoteMode{RemoteReadOnly, RemoteStandard, RemoteFull} {
			if ok, _ := remoteDecision(pattern, mode, nil); ok {
				t.Errorf("%s allowed remotely in mode %v", pattern, mode)
			}
		}
	}
}

// A wall clock that jumps must not make retention delete recent results.
func TestRetentionSurvivesClockJumps(t *testing.T) {
	now := time.Now()
	wr := NewWorkloadRegistry()
	finishedWorkload(wr, "just-now", "file.hash", domain.WorkloadCompleted, now)

	// Clock set back by a day, then by a year: results finished "in the
	// future" stay.
	for _, back := range []time.Duration{24 * time.Hour, 365 * 24 * time.Hour} {
		if got := wr.expireFinished(now.Add(-back), finishedRetention, 10, 5); len(got) != 0 {
			t.Fatalf("a clock set back %v expired %v", back, got)
		}
	}
	finishedWorkload(wr, "ancient", "file.hash", domain.WorkloadCompleted, now.Add(-30*24*time.Hour))
	// Forward by a day: only what retention already says is old goes.
	got := wr.expireFinished(now.Add(24*time.Hour), finishedRetention, 10, 5)
	if len(got) != 1 || got[0] != "ancient" {
		t.Fatalf("a day forward expired %v, want only the 30-day-old one", got)
	}
}
