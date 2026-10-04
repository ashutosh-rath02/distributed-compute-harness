package sysinfo

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// platformDeviceUse: power from "pmset -g batt", idle time from
// HIDIdleTime in "ioreg -c IOHIDSystem". Both are programs, so the
// answer is reused for a few seconds.
func platformDeviceUse(ctx context.Context) domain.DeviceUse {
	darwinUse.mu.Lock()
	defer darwinUse.mu.Unlock()
	if time.Since(darwinUse.at) < 10*time.Second {
		return darwinUse.last
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var u domain.DeviceUse
	if out, err := exec.CommandContext(ctx, "pmset", "-g", "batt").Output(); err == nil {
		u = parsePmset(string(out))
	}
	if out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem").Output(); err == nil {
		u.IdleSeconds = parseIoregIdle(string(out))
	}
	darwinUse.last, darwinUse.at = u, time.Now()
	return u
}

var darwinUse struct {
	mu   sync.Mutex
	at   time.Time
	last domain.DeviceUse
}
