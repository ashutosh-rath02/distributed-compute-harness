package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseByteSize parses a human-readable byte size like "2GiB", "512MB",
// "1024", or "1.5G" (case-insensitive, decimal and binary suffixes
// treated the same way). Empty is 0.
func ParseByteSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q: no numeric value", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}

	suffix := strings.ToUpper(strings.TrimSpace(s[i:]))
	var mult float64
	switch suffix {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("invalid size %q: unrecognized unit %q", s, suffix)
	}
	return uint64(n * mult), nil
}
