package domain

import (
	"fmt"
	"strings"
)

// Priority orders the work queue (manager queue.go): waiting high-priority
// work starts before normal before low, oldest first within one, and
// waiting raises a workload's priority over time so low priority can't
// starve. It only decides which waiting work starts next: running work is
// never stopped for it. Empty means normal, which is what every workload
// and job from before priorities has.
type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

// ParsePriority reads a priority as given on the API or the command line
// (any case): empty is normal, anything but high, normal or low an error.
func ParsePriority(s string) (Priority, error) {
	switch p := Priority(strings.ToLower(strings.TrimSpace(s))); p {
	case "":
		return PriorityNormal, nil
	case PriorityHigh, PriorityNormal, PriorityLow:
		return p, nil
	}
	return "", fmt.Errorf("priority must be high, normal or low, not %q", s)
}

// Canonical is p as high, normal or low: empty (work from before
// priorities) and anything unrecognized read as normal.
func (p Priority) Canonical() Priority {
	switch p {
	case PriorityHigh, PriorityLow:
		return p
	}
	return PriorityNormal
}

// Rank is p as a number for ordering: low 0, normal 1, high 2.
func (p Priority) Rank() int {
	switch p.Canonical() {
	case PriorityHigh:
		return 2
	case PriorityLow:
		return 0
	}
	return 1
}
