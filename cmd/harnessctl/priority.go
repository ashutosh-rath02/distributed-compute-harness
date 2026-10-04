package main

import (
	"flag"
	"fmt"

	"home-harness/internal/domain"
)

// priorityFlag adds -priority to a submit command (manager queue.go).
func priorityFlag(fs *flag.FlagSet) *string {
	return fs.String("priority", "normal", "high, normal or low: which waiting work starts first when every device is busy (waiting raises it over time; running work is never stopped)")
}

// checkPriority refuses a -priority the manager would refuse, before
// anything is uploaded.
func checkPriority(p string) (string, error) {
	parsed, err := domain.ParsePriority(p)
	if err != nil {
		return "", fmt.Errorf("-priority: %w", err)
	}
	return string(parsed), nil
}

// priorityNote marks work that isn't normal priority in a listing.
func priorityNote(p domain.Priority) string {
	if p.Canonical() == domain.PriorityNormal {
		return ""
	}
	return "  [" + string(p) + " priority]"
}
