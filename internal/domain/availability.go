package domain

// When a device takes new work: the operator's rule per device
// (Availability, operator-owned, in NodeMeta) read against what the device
// reports about its owner's use of it (DeviceUse, in each heartbeat).

// FeatureAvailability means the agent reports DeviceUse in its
// heartbeats. Until such an agent's first heartbeat its use is not known
// yet, and it takes no work; an agent without the feature is always
// treated as available, as before.
const FeatureAvailability = "availability.v1"

// DeviceUse is what a device reports about how it is being used. A nil
// field is unknown (no battery to read, no way to see input).
type DeviceUse struct {
	// OnBattery: running from its battery, not plugged in. False for a
	// device without a battery.
	OnBattery      *bool `json:"onBattery,omitempty"`
	BatteryPercent *int  `json:"batteryPercent,omitempty"`
	// IdleSeconds since the owner last used the keyboard, mouse or
	// screen (a phone: since its screen went off; 0 while it is on).
	IdleSeconds *int64 `json:"idleSeconds,omitempty"`
}

// AvailabilityMode names an availability rule.
type AvailabilityMode string

const (
	// AvailableAuto (the default): new work only while not on battery.
	AvailableAuto AvailabilityMode = "auto"
	// AvailableAlways: whenever connected.
	AvailableAlways AvailabilityMode = "always"
	// AvailableWhenIdle: only after IdleMinutes without input.
	AvailableWhenIdle AvailabilityMode = "idle"
	// AvailableWhenCharging: only while plugged in (same signal as auto,
	// stated explicitly — e.g. for a phone).
	AvailableWhenCharging AvailabilityMode = "charging"
	// AvailablePaused: no new work at all until changed.
	AvailablePaused AvailabilityMode = "paused"
)

// DefaultIdleMinutes is the idle rule's wait when none is set.
const DefaultIdleMinutes = 5

// Availability is the operator's rule for when a device takes new work.
// Work already running always finishes; the rule only holds back new work.
type Availability struct {
	Mode AvailabilityMode `json:"mode,omitempty"`
	// IdleMinutes without input before an "idle" device takes work.
	IdleMinutes int `json:"idleMinutes,omitempty"`
	// Hours optionally limits any mode to a daily window, "HH:MM-HH:MM"
	// in the manager's local time (it may wrap past midnight).
	Hours string `json:"hours,omitempty"`
}

// IsDefault reports whether a is the rule every device has unset.
func (a Availability) IsDefault() bool {
	return (a.Mode == "" || a.Mode == AvailableAuto) && a.Hours == ""
}
