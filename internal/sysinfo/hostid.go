package sysinfo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"
	"strings"

	"github.com/shirou/gopsutil/v4/host"
)

// Host fingerprint sources.
const (
	// HostSourceMachine: derived from a persistent machine ID (Windows
	// MachineGuid, Linux product_uuid or machine-id, macOS IOPlatformUUID).
	HostSourceMachine = "machine"
	// HostSourceBoot: derived from Linux's per-boot ID because nothing
	// persistent was readable — typical under Android/Termux. Two agents
	// on one device still match each other, but the value changes every
	// reboot.
	HostSourceBoot = "boot"
)

// HostFingerprint returns a privacy-preserving identifier for the physical
// (or virtual) machine this agent runs on, so the manager can notice two
// node identities on one machine and avoid counting its capacity twice.
// Only a salted SHA-256 prefix ever leaves the machine, never the raw ID.
// Returns ("", "") if no host ID is available.
//
// It is a hint, not a proof: the agent asserts it, cloned VMs and container
// images can share machine IDs, and a hostile agent could copy another's.
// The manager only acts on it for display and corroborated capacity
// totals, never for anything automatic.
func HostFingerprint(ctx context.Context) (fingerprint, source string) {
	raw, err := host.HostIDWithContext(ctx)
	raw = strings.ToLower(strings.TrimSpace(raw))
	if err != nil || raw == "" {
		return "", ""
	}
	source = HostSourceMachine
	if runtime.GOOS == "linux" && !stableLinuxHostID() {
		source = HostSourceBoot
	}
	sum := sha256.Sum256([]byte("home-harness-host-v1:" + raw))
	return hex.EncodeToString(sum[:])[:32], source
}

// stableLinuxHostID mirrors the order gopsutil's HostID tries on Linux,
// reporting whether it could use a persistent ID rather than falling back
// to /proc/sys/kernel/random/boot_id.
func stableLinuxHostID() bool {
	if b, err := os.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil && strings.TrimSpace(string(b)) != "" {
		return true
	}
	if b, err := os.ReadFile("/etc/machine-id"); err == nil && len(strings.TrimSpace(string(b))) == 32 {
		return true
	}
	return false
}
