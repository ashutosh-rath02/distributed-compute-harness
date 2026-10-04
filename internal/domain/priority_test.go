package domain

import "testing"

func TestParsePriority(t *testing.T) {
	for in, want := range map[string]Priority{"": PriorityNormal, "high": PriorityHigh, " LOW ": PriorityLow, "Normal": PriorityNormal} {
		if got, err := ParsePriority(in); err != nil || got != want {
			t.Errorf("ParsePriority(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"urgent", "1", "highest", "<b>"} {
		if _, err := ParsePriority(bad); err == nil {
			t.Errorf("ParsePriority(%q) accepted", bad)
		}
	}
	if PriorityHigh.Rank() <= PriorityNormal.Rank() || PriorityNormal.Rank() <= PriorityLow.Rank() {
		t.Fatal("ranks must order high > normal > low")
	}
	if Priority("").Rank() != PriorityNormal.Rank() || Priority("bogus").Canonical() != PriorityNormal {
		t.Fatal("empty and unknown priorities must read as normal")
	}
}
