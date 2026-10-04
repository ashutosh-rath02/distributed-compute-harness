package sysinfo

import (
	"context"
	"testing"
)

func TestPowerFromStatus(t *testing.T) {
	for name, c := range map[string]struct {
		st   systemPowerStatus
		want string
	}{
		"desktop, no battery": {systemPowerStatus{ACLineStatus: 1, BatteryFlag: 128, BatteryLifePercent: 255}, "battery=false pct=? idle=?"},
		"laptop unplugged":    {systemPowerStatus{ACLineStatus: 0, BatteryFlag: 1, BatteryLifePercent: 77}, "battery=true pct=77 idle=?"},
		"laptop plugged in":   {systemPowerStatus{ACLineStatus: 1, BatteryFlag: 8, BatteryLifePercent: 75}, "battery=false pct=75 idle=?"},
		"unknown":             {systemPowerStatus{ACLineStatus: 255, BatteryFlag: 255, BatteryLifePercent: 255}, "battery=? pct=? idle=?"},
	} {
		if got := describe(powerFromStatus(c.st)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// On this PC: power and idle time are readable at all.
func TestPlatformDeviceUseReadsThisPC(t *testing.T) {
	u := platformDeviceUse(context.Background())
	if u.OnBattery == nil || u.IdleSeconds == nil || *u.IdleSeconds < 0 {
		t.Fatalf("this Windows PC: %s", describe(u))
	}
	t.Logf("this PC: %s", describe(u))
}
