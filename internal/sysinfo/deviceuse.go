package sysinfo

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"home-harness/internal/domain"
)

// DeviceUse reports how the device is being used — on battery or not,
// how long since its owner last touched it — for the operator's
// availability rule (domain.Availability). Whatever can't be read stays
// nil (unknown), which never holds a device back.
//
// stateFile, when set, is a file another program keeps current with what
// this process can't see for itself: the Android app writes the phone's
// charging and screen state there (an app's Go child process can't ask
// Android). While fresh it overrides the values it carries.
func DeviceUse(ctx context.Context, stateFile string) domain.DeviceUse {
	u := platformDeviceUse(ctx)
	if stateFile != "" {
		if s, ok := readStateFile(stateFile, time.Now()); ok {
			if s.OnBattery != nil {
				u.OnBattery = s.OnBattery
			}
			if s.BatteryPercent != nil {
				u.BatteryPercent = s.BatteryPercent
			}
			if s.IdleSeconds != nil {
				u.IdleSeconds = s.IdleSeconds
			}
		}
	}
	return u
}

// stateFileMaxAge: older than this, the writer has stopped (app killed)
// and the file says nothing. The app rewrites it every 30 s.
const stateFileMaxAge = 2 * time.Minute

// readStateFile parses a device-state file: key=value lines
//
//	updated=<unix seconds>   when it was written (required)
//	on_battery=0|1
//	battery=<percent>
//	screen_on=0|1
//	screen_off_since=<unix seconds>
//
// Idle time is 0 while the screen is on, else the time since it went off.
func readStateFile(path string, now time.Time) (domain.DeviceUse, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.DeviceUse{}, false
	}
	return parseStateFile(string(data), now)
}

func parseStateFile(text string, now time.Time) (domain.DeviceUse, bool) {
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	updated, err := strconv.ParseInt(kv["updated"], 10, 64)
	if err != nil {
		return domain.DeviceUse{}, false
	}
	if age := now.Sub(time.Unix(updated, 0)); age > stateFileMaxAge || age < -stateFileMaxAge {
		return domain.DeviceUse{}, false
	}
	var u domain.DeviceUse
	switch kv["on_battery"] {
	case "0", "1":
		b := kv["on_battery"] == "1"
		u.OnBattery = &b
	}
	if p, err := strconv.Atoi(kv["battery"]); err == nil && p >= 0 && p <= 100 {
		u.BatteryPercent = &p
	}
	switch kv["screen_on"] {
	case "1":
		zero := int64(0)
		u.IdleSeconds = &zero
	case "0":
		if since, err := strconv.ParseInt(kv["screen_off_since"], 10, 64); err == nil {
			idle := int64(now.Sub(time.Unix(since, 0)).Seconds())
			if idle < 0 {
				idle = 0
			}
			u.IdleSeconds = &idle
		}
	}
	return u, true
}

// readPowerSupply reads Linux's /sys/class/power_supply (root): on
// battery when a battery is discharging; a device with only mains or USB
// supplies is plugged in. Unknown when there is nothing to read.
func readPowerSupply(root string) domain.DeviceUse {
	var u domain.DeviceUse
	entries, err := os.ReadDir(root)
	if err != nil {
		return u
	}
	read := func(dir, name string) string {
		b, err := os.ReadFile(filepath.Join(root, dir, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	sawMains, sawBattery := false, false
	for _, e := range entries {
		switch read(e.Name(), "type") {
		case "Battery":
			if read(e.Name(), "scope") == "Device" {
				continue // a mouse's or headset's battery, not the system's
			}
			status := read(e.Name(), "status")
			if status == "" {
				continue
			}
			sawBattery = true
			onBattery := status == "Discharging"
			u.OnBattery = &onBattery
			if p, err := strconv.Atoi(read(e.Name(), "capacity")); err == nil && p >= 0 && p <= 100 {
				u.BatteryPercent = &p
			}
		case "Mains", "USB", "USB_C", "USB_PD":
			sawMains = true
		}
	}
	if !sawBattery && sawMains {
		f := false
		u.OnBattery = &f
	}
	return u
}

var (
	pmsetSource  = regexp.MustCompile(`Now drawing from '([^']+)'`)
	pmsetPercent = regexp.MustCompile(`(\d{1,3})%`)
	ioregIdle    = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)
)

// parsePmset reads macOS "pmset -g batt": the power source and the
// internal battery's charge.
func parsePmset(out string) domain.DeviceUse {
	var u domain.DeviceUse
	if m := pmsetSource.FindStringSubmatch(out); m != nil {
		onBattery := m[1] == "Battery Power"
		u.OnBattery = &onBattery
	}
	if strings.Contains(out, "InternalBattery") {
		if m := pmsetPercent.FindStringSubmatch(out); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil && p <= 100 {
				u.BatteryPercent = &p
			}
		}
	}
	return u
}

// parseIoregIdle reads HIDIdleTime (nanoseconds) from "ioreg -c
// IOHIDSystem" as seconds since the last input.
func parseIoregIdle(out string) *int64 {
	m := ioregIdle.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	ns, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil
	}
	s := ns / int64(time.Second)
	return &s
}
