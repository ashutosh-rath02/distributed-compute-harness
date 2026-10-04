package agent

import (
	"context"
	"log"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/keepawake"
	"home-harness/internal/sysinfo"
)

// deviceUse is what the heartbeat reports about the device's use, for
// the operator's availability rule on the manager.
func (a *Agent) deviceUse(ctx context.Context) domain.DeviceUse {
	if a.cfg.DeviceUse != nil {
		return a.cfg.DeviceUse(ctx)
	}
	return sysinfo.DeviceUse(ctx, a.cfg.DeviceStateFile)
}

// gpus is what the manifest reports about the device's GPUs.
func (a *Agent) gpus(ctx context.Context) []domain.GPU {
	if a.cfg.GPUs != nil {
		return a.cfg.GPUs(ctx)
	}
	return sysinfo.GPUs(ctx)
}

// keepAwakeWhileWorking holds a keep-awake request while any workload
// runs here, checked every few seconds.
func (a *Agent) keepAwakeWhileWorking(ctx context.Context) {
	req := keepawake.New("Home Harness agent: running a task for your devices")
	defer req.Hold(false)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		if err := req.Hold(a.executor.Running() > 0); err != nil {
			log.Printf("agent %s: keep-awake: %v", a.identity.NodeID, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
