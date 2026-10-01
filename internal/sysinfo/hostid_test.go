package sysinfo

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/shirou/gopsutil/v4/host"
)

func TestHostFingerprintIsAStableSaltedHash(t *testing.T) {
	ctx := context.Background()
	fp, source := HostFingerprint(ctx)
	if fp == "" {
		t.Skip("no host ID available on this machine")
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(fp) {
		t.Fatalf("fingerprint %q is not a 32-hex-char hash prefix", fp)
	}
	if source != HostSourceMachine && source != HostSourceBoot {
		t.Fatalf("unexpected source %q", source)
	}
	if again, _ := HostFingerprint(ctx); again != fp {
		t.Fatal("fingerprint must be stable across calls")
	}
	raw, _ := host.HostIDWithContext(ctx)
	compact := strings.ToLower(strings.ReplaceAll(raw, "-", ""))
	if raw != "" && (strings.Contains(fp, strings.ToLower(raw)) || (len(compact) >= 8 && strings.Contains(fp, compact[:8]))) {
		t.Fatal("the fingerprint must not reveal the raw machine ID")
	}
}
