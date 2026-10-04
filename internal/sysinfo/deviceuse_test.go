package sysinfo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func describe(u domain.DeviceUse) string {
	s := func(b *bool) string {
		if b == nil {
			return "?"
		}
		return fmt.Sprint(*b)
	}
	i := func(p *int) string {
		if p == nil {
			return "?"
		}
		return fmt.Sprint(*p)
	}
	idle := "?"
	if u.IdleSeconds != nil {
		idle = fmt.Sprint(*u.IdleSeconds)
	}
	return "battery=" + s(u.OnBattery) + " pct=" + i(u.BatteryPercent) + " idle=" + idle
}

func TestParseStateFile(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for text, want := range map[string]string{
		"updated=1000000\non_battery=1\nbattery=63\nscreen_on=1\n":             "battery=true pct=63 idle=0",
		"updated=999990\non_battery=0\nscreen_on=0\nscreen_off_since=999400\n": "battery=false pct=? idle=600",
		"updated=1000000\nscreen_on=0\n":                                       "battery=? pct=? idle=?",
		" updated = 1000000 \n on_battery = 1 \n battery=250\n":                "battery=true pct=? idle=?",
		"updated=1000000\nscreen_on=0\nscreen_off_since=1000100\n":             "battery=? pct=? idle=0",
	} {
		u, ok := parseStateFile(text, now)
		if !ok || describe(u) != want {
			t.Errorf("%q: %v %s, want %s", text, ok, describe(u), want)
		}
	}
	// A writer that stopped (or never said when it wrote) says nothing.
	for _, text := range []string{"on_battery=1\n", "updated=999000\non_battery=1\n", "updated=soon\n", ""} {
		if _, ok := parseStateFile(text, now); ok {
			t.Errorf("%q accepted", text)
		}
	}
}

func TestDeviceUseStateFileOverridesWhileFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-state")
	write := func(updated time.Time) {
		os.WriteFile(path, []byte(fmt.Sprintf("updated=%d\non_battery=1\nbattery=12\nscreen_on=1\n", updated.Unix())), 0o600)
	}
	write(time.Now())
	u := DeviceUse(context.Background(), path)
	if u.OnBattery == nil || !*u.OnBattery || *u.BatteryPercent != 12 || *u.IdleSeconds != 0 {
		t.Fatalf("fresh file: %s", describe(u))
	}
	write(time.Now().Add(-10 * time.Minute))
	if u := DeviceUse(context.Background(), path); u.BatteryPercent != nil && *u.BatteryPercent == 12 {
		t.Fatalf("stale file still used: %s", describe(u))
	}
}

func TestReadPowerSupply(t *testing.T) {
	tree := func(t *testing.T, files map[string]string) string {
		root := t.TempDir()
		for name, content := range files {
			p := filepath.Join(root, filepath.FromSlash(name))
			os.MkdirAll(filepath.Dir(p), 0o700)
			os.WriteFile(p, []byte(content+"\n"), 0o600)
		}
		return root
	}
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"laptop on battery": {map[string]string{"BAT0/type": "Battery", "BAT0/status": "Discharging", "BAT0/capacity": "57", "AC/type": "Mains", "AC/online": "0"}, "battery=true pct=57 idle=?"},
		"laptop charging":   {map[string]string{"BAT0/type": "Battery", "BAT0/status": "Charging", "BAT0/capacity": "80"}, "battery=false pct=80 idle=?"},
		"phone full":        {map[string]string{"battery/type": "Battery", "battery/status": "Full", "battery/capacity": "100", "usb/type": "USB"}, "battery=false pct=100 idle=?"},
		"desktop, mains":    {map[string]string{"ACAD/type": "Mains", "ACAD/online": "1"}, "battery=false pct=? idle=?"},
		"a mouse's battery": {map[string]string{"hidpp_battery_0/type": "Battery", "hidpp_battery_0/scope": "Device", "hidpp_battery_0/status": "Discharging"}, "battery=? pct=? idle=?"},
		"nothing":           {map[string]string{}, "battery=? pct=? idle=?"},
	} {
		if got := describe(readPowerSupply(tree(t, c.files))); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if got := describe(readPowerSupply(filepath.Join(t.TempDir(), "missing"))); got != "battery=? pct=? idle=?" {
		t.Errorf("no power_supply: %s", got)
	}
}

func TestParseMacOutputs(t *testing.T) {
	onBattery := "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1234567)\t71%; discharging; 4:12 remaining present: true\n"
	if got := describe(parsePmset(onBattery)); got != "battery=true pct=71 idle=?" {
		t.Errorf("on battery: %s", got)
	}
	mini := "Now drawing from 'AC Power'\n"
	if got := describe(parsePmset(mini)); got != "battery=false pct=? idle=?" {
		t.Errorf("Mac mini: %s", got)
	}
	ioreg := `    | |   "HIDIdleTime" = 125000000000` + "\n"
	if got := parseIoregIdle(ioreg); got == nil || *got != 125 {
		t.Errorf("idle: %v", got)
	}
	if parseIoregIdle("nothing here") != nil {
		t.Error("idle from nothing")
	}
}
