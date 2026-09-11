package domain

import "testing"

func TestWantsRestartAfter(t *testing.T) {
	cases := []struct {
		policy RestartPolicy
		state  WorkloadState
		want   bool
	}{
		// RestartNever: never restarts, regardless of state.
		{RestartNever, WorkloadCompleted, false},
		{RestartNever, WorkloadFailed, false},
		{RestartNever, WorkloadUnknown, false},
		{RestartNever, WorkloadCanceled, false},

		// RestartOnFailure: only FAILED/UNKNOWN, not a clean COMPLETED.
		{RestartOnFailure, WorkloadCompleted, false},
		{RestartOnFailure, WorkloadFailed, true},
		{RestartOnFailure, WorkloadUnknown, true},
		{RestartOnFailure, WorkloadCanceled, false},

		// RestartAlways: any terminal state except CANCELED.
		{RestartAlways, WorkloadCompleted, true},
		{RestartAlways, WorkloadFailed, true},
		{RestartAlways, WorkloadUnknown, true},
		{RestartAlways, WorkloadCanceled, false},

		// PENDING/RUNNING are never restart-eligible, even under Always —
		// already in flight.
		{RestartAlways, WorkloadPending, false},
		{RestartAlways, WorkloadRunning, false},
	}

	for _, c := range cases {
		if got := c.policy.WantsRestartAfter(c.state); got != c.want {
			t.Errorf("RestartPolicy(%q).WantsRestartAfter(%q) = %v, want %v", c.policy, c.state, got, c.want)
		}
	}
}

func TestParseRestartPolicy(t *testing.T) {
	cases := []struct {
		in   string
		want RestartPolicy
	}{
		{"", RestartNever},
		{"never", RestartNever},
		{"on-failure", RestartOnFailure},
		{"always", RestartAlways},
	}
	for _, c := range cases {
		got, err := ParseRestartPolicy(c.in)
		if err != nil {
			t.Fatalf("ParseRestartPolicy(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseRestartPolicy(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseRestartPolicyRejectsUnknown(t *testing.T) {
	if _, err := ParseRestartPolicy("sometimes"); err == nil {
		t.Fatal("expected an error for an unrecognized restart policy string")
	}
}
