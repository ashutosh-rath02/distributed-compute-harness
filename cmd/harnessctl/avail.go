package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"home-harness/internal/domain"
)

// cmdAvail shows or sets when a device takes new work.
func (c *apiClient) cmdAvail(id string, args []string) error {
	var n nodeView
	if err := c.get("/nodes/"+id, &n); err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Printf("%s takes work: %s\n", n.displayName(), describeAvailability(n))
		return nil
	}
	rule := n.Availability.Rule
	if rule.Mode == "" {
		rule.Mode = domain.AvailableAuto
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "auto", "always", "charging", "paused":
			rule.Mode, rule.IdleMinutes = domain.AvailabilityMode(a), 0
		case "idle":
			rule.Mode, rule.IdleMinutes = domain.AvailableWhenIdle, 0
			if i+1 < len(args) {
				if m, err := strconv.Atoi(args[i+1]); err == nil {
					rule.IdleMinutes = m
					i++
				}
			}
		case "hours":
			if i+1 >= len(args) {
				return fmt.Errorf("hours needs a window like 22:00-07:00 (or - to remove it)")
			}
			rule.Hours = args[i+1]
			if rule.Hours == "-" {
				rule.Hours = ""
			}
			i++
		default:
			return fmt.Errorf("unknown %q: want auto, always, idle [minutes], charging, paused, or hours HH:MM-HH:MM", a)
		}
	}
	body, _ := json.Marshal(rule)
	req, err := http.NewRequest(http.MethodPut, c.base+"/nodes/"+id+"/availability", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("set availability: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&n.Availability); err != nil {
		return err
	}
	fmt.Printf("%s takes work: %s\n", n.displayName(), describeAvailability(n))
	return nil
}

// describeAvailability says the rule and whether it lets work in now.
func describeAvailability(n nodeView) string {
	r := n.Availability.Rule
	var rule string
	switch r.Mode {
	case domain.AvailableAlways:
		rule = "always"
	case domain.AvailableWhenIdle:
		m := r.IdleMinutes
		if m <= 0 {
			m = domain.DefaultIdleMinutes
		}
		rule = fmt.Sprintf("after %d min without use", m)
	case domain.AvailableWhenCharging:
		rule = "only while charging"
	case domain.AvailablePaused:
		rule = "paused"
	default:
		rule = "auto (not while on battery)"
	}
	if r.Hours != "" {
		rule += ", only " + r.Hours
	}
	now := "yes, now"
	if !n.Availability.Available {
		now = "not now: " + n.Availability.Reason
	} else if n.Availability.Reason != "" {
		now += " (" + n.Availability.Reason + ")"
	}
	return rule + " — " + now
}

// describeUse reads out what the device last reported about its use.
func describeUse(u domain.DeviceUse) string {
	var parts []string
	switch {
	case u.OnBattery == nil:
		parts = append(parts, "power unknown")
	case *u.OnBattery:
		parts = append(parts, "on battery")
	default:
		parts = append(parts, "plugged in")
	}
	if u.BatteryPercent != nil {
		parts = append(parts, fmt.Sprintf("battery %d%%", *u.BatteryPercent))
	}
	if u.IdleSeconds != nil {
		parts = append(parts, fmt.Sprintf("idle %ds", *u.IdleSeconds))
	} else {
		parts = append(parts, "idle time unknown")
	}
	return strings.Join(parts, ", ")
}
