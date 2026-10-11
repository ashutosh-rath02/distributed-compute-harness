// Package cronspec reads schedule expressions and finds their next slot:
// the five-field cron form ("*/15 9-17 * * mon-fri"), the macros
// @hourly @daily @midnight @weekly @monthly @yearly @annually, and
// "every 15m" (an interval counted from an anchor instant).
//
// Cron fields are matched against wall-clock time in the schedule's
// time zone. At a daylight-saving change that means:
//   - a time that does not exist (02:30 when the clocks jump from 02:00 to
//     03:00) fires once, right after the jump (as 03:30);
//   - a repeated hour fires on its first pass only, never twice.
//
// "every" intervals are plain elapsed time and ignore the zone.
//
// Day of month and day of week follow the usual cron rule: when both are
// restricted a day matches if either does; when one is "*", only the
// other counts.
package cronspec

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // zone names work on machines without a zone database (Windows, Android)
)

// MinInterval and MaxInterval bound an "every" schedule.
const (
	MinInterval = time.Minute
	MaxInterval = 366 * 24 * time.Hour
	// maxSearchYears: an expression with no slot this far ahead (0 0 30 2 *)
	// never matches and is refused.
	maxSearchYears = 9
	maxExprLen     = 200
)

// Spec is a parsed expression.
type Spec struct {
	interval time.Duration // > 0: an "every" schedule
	anchor   time.Time
	loc      *time.Location

	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
}

// LoadLocation resolves an IANA zone name ("" is UTC). The manager's own
// local zone ("Local") is refused: it would mean something different on
// the next machine the state is moved to.
func LoadLocation(name string) (*time.Location, error) {
	if name == "" || strings.EqualFold(name, "utc") {
		return time.UTC, nil
	}
	if strings.EqualFold(name, "local") {
		return nil, errors.New(`use an IANA zone name such as "Europe/Berlin" or "America/New_York", not "Local"`)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q (use an IANA name such as Europe/Berlin)", name)
	}
	return loc, nil
}

// Parse reads expr. anchor is where "every" intervals count from
// (normally when the schedule was made).
func Parse(expr string, anchor time.Time, loc *time.Location) (*Spec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" || len(expr) > maxExprLen {
		return nil, fmt.Errorf("a schedule is 1-%d characters", maxExprLen)
	}
	s := &Spec{anchor: anchor.UTC().Round(0), loc: loc}
	low := strings.ToLower(expr)
	switch {
	case strings.HasPrefix(low, "every "):
		d, err := parseEvery(strings.TrimSpace(low[len("every "):]))
		if err != nil {
			return nil, err
		}
		s.interval = d
		return s, nil
	case strings.HasPrefix(low, "@"):
		fields, ok := map[string]string{
			"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *",
			"@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
		}[low]
		if !ok {
			return nil, fmt.Errorf("unknown %q (@hourly, @daily, @weekly, @monthly, @yearly, or write the five fields)", expr)
		}
		expr = fields
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("%q: expected five fields (minute hour day-of-month month day-of-week), an @macro or \"every 15m\"", expr)
	}
	var err error
	if s.minute, _, err = parseField(parts[0], "minute", 0, 59, nil); err != nil {
		return nil, err
	}
	if s.hour, _, err = parseField(parts[1], "hour", 0, 23, nil); err != nil {
		return nil, err
	}
	if s.dom, s.domStar, err = parseField(parts[2], "day of month", 1, 31, nil); err != nil {
		return nil, err
	}
	months := map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	if s.month, _, err = parseField(parts[3], "month", 1, 12, months); err != nil {
		return nil, err
	}
	days := map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	if s.dow, s.dowStar, err = parseField(parts[4], "day of week", 0, 7, days); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
		s.dow &^= 1 << 7
	}
	if _, ok := s.Next(anchor); !ok {
		return nil, fmt.Errorf("%q never matches a real date", expr)
	}
	return s, nil
}

func parseEvery(v string) (time.Duration, error) {
	// "15m", "2h", "1d", "1h30m".
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 1 {
			return 0, fmt.Errorf("every %q: use a duration like 15m, 2h or 1d", v)
		}
		v = strconv.Itoa(n*24) + "h"
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("every %q: use a duration like 15m, 2h or 1d", v)
	}
	if d < MinInterval || d > MaxInterval {
		return 0, fmt.Errorf("every %s: between %s and %d days", v, MinInterval, int(MaxInterval/(24*time.Hour)))
	}
	return d, nil
}

// parseField reads one cron field into a bit set; star reports a bare "*".
func parseField(f, what string, lo, hi int, names map[string]int) (set uint64, star bool, err error) {
	bad := func(msg string) (uint64, bool, error) {
		return 0, false, fmt.Errorf("%s %q: %s", what, f, msg)
	}
	if f == "" {
		return bad("empty")
	}
	for _, item := range strings.Split(f, ",") {
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, convErr := strconv.Atoi(stepText)
			if convErr != nil || n < 1 || n > hi {
				return bad("a step is a whole number from 1")
			}
			step = n
		}
		var a, b int
		switch {
		case rng == "*":
			a, b = lo, hi
			if item == "*" && f == "*" {
				star = true
			}
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			var e1, e2 error
			a, e1 = number(x, names)
			b, e2 = number(y, names)
			if e1 != nil || e2 != nil {
				return bad("not a number in range")
			}
		default:
			var e error
			if a, e = number(rng, names); e != nil {
				return bad("not a number in range")
			}
			b = a
			if hasStep {
				b = hi // "5/15" = from 5 to the end, every 15
			}
		}
		if a < lo || b > hi || a > b {
			return bad(fmt.Sprintf("must be within %d-%d", lo, hi))
		}
		for v := a; v <= b; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, star, nil
}

func number(s string, names map[string]int) (int, error) {
	if n, ok := names[strings.ToLower(s)]; ok {
		return n, nil
	}
	if len(s) == 0 || len(s) > 2 {
		return 0, errors.New("bad number")
	}
	return strconv.Atoi(s)
}

// Next returns the first slot strictly after the given instant.
func (s *Spec) Next(after time.Time) (time.Time, bool) {
	after = after.UTC().Round(0) // wall clock only: drop the monotonic reading
	if s.interval > 0 {
		if after.Before(s.anchor) {
			return s.anchor.Add(s.interval), true
		}
		k := after.Sub(s.anchor)/s.interval + 1
		return s.anchor.Add(k * s.interval), true
	}
	w := after.In(s.loc)
	// Count in a plain calendar (UTC arithmetic over wall-clock fields) and
	// only turn a candidate into an instant at the end, so a wall time that
	// the zone skips is not silently renormalized away.
	t := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, time.UTC).Add(time.Minute)
	limit := t.AddDate(maxSearchYears, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		c := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, s.loc)
		if cw := c.In(s.loc); cw.Hour() != t.Hour() || cw.Minute() != t.Minute() {
			// The zone skips this wall time. Go answers with the instant an
			// hour (the jump) too early; move forward to where that wall
			// time lands after the jump.
			_, before := c.In(s.loc).Zone()
			_, afterJump := c.Add(3 * time.Hour).In(s.loc).Zone()
			c = c.Add(time.Duration(afterJump-before) * time.Second)
		}
		if c.After(after) {
			return c.UTC(), true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

func (s *Spec) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	}
	return domOK || dowOK
}
