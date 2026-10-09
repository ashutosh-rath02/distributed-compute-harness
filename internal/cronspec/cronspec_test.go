package cronspec

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func parse(t *testing.T, expr, zone string) *Spec {
	t.Helper()
	s, err := Parse(expr, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), mustLoc(t, zone))
	if err != nil {
		t.Fatalf("%q: %v", expr, err)
	}
	return s
}

func at(t *testing.T, zone string, y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, mustLoc(t, zone))
}

func TestNextBasics(t *testing.T) {
	cases := []struct {
		expr  string
		after time.Time
		want  time.Time
	}{
		{"*/15 * * * *", time.Date(2026, 5, 1, 10, 7, 30, 0, time.UTC), time.Date(2026, 5, 1, 10, 15, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 5, 1, 10, 15, 0, 0, time.UTC), time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)}, // strictly after
		{"@hourly", time.Date(2026, 5, 1, 10, 59, 0, 0, time.UTC), time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)},
		{"@daily", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC), time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)},
		{"0 9-17/4 * * mon-fri", time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC), time.Date(2026, 5, 1, 17, 0, 0, 0, time.UTC)}, // Fri 1 May 2026
		{"0 9 * * mon-fri", time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC), time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)},       // over the weekend
		{"30 4 1,15 * *", time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 15, 4, 30, 0, 0, time.UTC)},
		{"0 0 29 2 *", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"0 0 * * 7", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)}, // 7 = Sunday
		{"@monthly", time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"5/20 * * * *", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC), time.Date(2026, 5, 1, 10, 5, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, ok := parse(t, c.expr, "UTC").Next(c.after)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q after %v: got %v (%v), want %v", c.expr, c.after, got, ok, c.want)
		}
	}
}

// With both day fields restricted a day matches if either does.
func TestDayOfMonthOrDayOfWeek(t *testing.T) {
	s := parse(t, "0 0 13 * fri", "UTC")
	var got []int
	after := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for len(got) < 6 {
		n, _ := s.Next(after)
		got = append(got, n.Day())
		after = n
	}
	// Fridays of Feb 2026: 6, 13, 20, 27; and the 13th (a Friday: once).
	want := []int{6, 13, 20, 27, 6, 13}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("days %v, want %v", got, want)
		}
	}
}

func TestEvery(t *testing.T) {
	anchor := time.Date(2026, 5, 1, 10, 3, 0, 0, time.UTC)
	s, err := Parse("every 15m", anchor, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := s.Next(anchor.Add(14 * time.Minute))
	if !n.Equal(anchor.Add(15 * time.Minute)) {
		t.Fatalf("got %v", n)
	}
	n, _ = s.Next(anchor.Add(15 * time.Minute)) // strictly after
	if !n.Equal(anchor.Add(30 * time.Minute)) {
		t.Fatalf("got %v", n)
	}
	n, _ = s.Next(anchor.Add(-time.Hour)) // before the anchor
	if !n.Equal(anchor.Add(15 * time.Minute)) {
		t.Fatalf("before the anchor: %v", n)
	}
	for _, ok := range []string{"every 1m", "every 2h", "every 1d", "every 1h30m", "EVERY 5M"} {
		if _, err := Parse(ok, anchor, time.UTC); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestParseRefusals(t *testing.T) {
	anchor := time.Now()
	for expr, want := range map[string]string{
		"":                       "1-200",
		"* * * *":                "five fields",
		"* * * * * *":            "five fields",
		"60 * * * *":             "within 0-59",
		"* 24 * * *":             "within 0-23",
		"* * 0 * *":              "within 1-31",
		"* * * 13 *":             "within 1-12",
		"* * * * 8":              "within 0-7",
		"5-1 * * * *":            "within",
		"*/0 * * * *":            "step",
		"a * * * *":              "not a number",
		"0 0 30 2 *":             "never matches",
		"0 0 31 4 *":             "never matches",
		"@sometimes":             "unknown",
		"every 10s":              "between",
		"every 400d":             "between",
		"every banana":           "duration",
		"every 0d":               "duration",
		strings.Repeat("1", 300): "1-200",
	} {
		if _, err := Parse(expr, anchor, time.UTC); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", expr, err, want)
		}
	}
	if _, err := LoadLocation("Mars/Base"); err == nil {
		t.Error("unknown zone accepted")
	}
	if _, err := LoadLocation("Local"); err == nil {
		t.Error("Local accepted")
	}
}

// Spring forward (New York, 8 March 2026: 02:00 -> 03:00): a 02:30 job
// fires once, just after the jump, and the day after fires at 02:30 again.
func TestDSTGap(t *testing.T) {
	const z = "America/New_York"
	s := parse(t, "30 2 * * *", z)
	after := at(t, z, 2026, 3, 7, 2, 30)
	first, _ := s.Next(after)
	if want := at(t, z, 2026, 3, 8, 3, 30); !first.Equal(want) {
		t.Fatalf("gap day: %v, want %v", first.In(mustLoc(t, z)), want)
	}
	next, _ := s.Next(first)
	if want := at(t, z, 2026, 3, 9, 2, 30); !next.Equal(want) {
		t.Fatalf("day after: %v, want %v", next.In(mustLoc(t, z)), want)
	}
	// An hourly :30 job doesn't fire twice at 03:30.
	h := parse(t, "30 * * * *", z)
	var seen []time.Time
	cur := at(t, z, 2026, 3, 8, 0, 45)
	for i := 0; i < 4; i++ {
		cur, _ = h.Next(cur)
		seen = append(seen, cur)
	}
	for i := 1; i < len(seen); i++ {
		if !seen[i].After(seen[i-1]) {
			t.Fatalf("not increasing: %v", seen)
		}
	}
	if seen[0] != at(t, z, 2026, 3, 8, 1, 30).UTC() || seen[1] != at(t, z, 2026, 3, 8, 3, 30).UTC() || seen[2] != at(t, z, 2026, 3, 8, 4, 30).UTC() {
		t.Fatalf("hourly across the gap: %v", seen)
	}
}

// Fall back (1 November 2026: 02:00 EDT -> 01:00 EST): 01:30 happens
// twice; a daily 01:30 job fires on the first pass only, once.
func TestDSTRepeatedHour(t *testing.T) {
	const z = "America/New_York"
	s := parse(t, "30 1 * * *", z)
	first, _ := s.Next(at(t, z, 2026, 10, 31, 1, 30))
	if first.Sub(at(t, z, 2026, 10, 31, 1, 30)) != 24*time.Hour {
		t.Fatalf("first: %v", first.In(mustLoc(t, z)))
	}
	second, _ := s.Next(first)
	if _, off := second.In(mustLoc(t, z)).Zone(); off != -5*3600 {
		t.Fatalf("expected EST (the second 01:30 is not a slot twice): %v", second.In(mustLoc(t, z)))
	}
	next, _ := s.Next(second)
	if next.Sub(second) < 23*time.Hour {
		t.Fatalf("fired twice in one night: %v then %v", second, next)
	}
	// Whatever the zone does, slots only ever move forward.
	h := parse(t, "0 * * * *", z)
	cur := at(t, z, 2026, 11, 1, 0, 0)
	for i := 0; i < 8; i++ {
		n, ok := h.Next(cur)
		if !ok || !n.After(cur) {
			t.Fatalf("went backwards: %v after %v", n, cur)
		}
		cur = n
	}
}

// The monotonic reading of time.Now() must not leak into the slots.
func TestNextIgnoresMonotonicReading(t *testing.T) {
	now := time.Now()
	s := parse(t, "* * * * *", "UTC")
	n, _ := s.Next(now)
	if n.Before(now.Round(0)) || n.Sub(now.Round(0)) > time.Minute {
		t.Fatalf("next %v for now %v", n, now)
	}
}
