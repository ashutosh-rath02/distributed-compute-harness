package sysinfo

import (
	"context"

	"home-harness/internal/domain"
)

// platformDeviceUse: power from /sys/class/power_supply. Idle time isn't
// visible to a background process on Linux (no portable way past X11 or
// Wayland); the Android app supplies it through the state file.
func platformDeviceUse(context.Context) domain.DeviceUse {
	return readPowerSupply("/sys/class/power_supply")
}
