// Package sysinfo collects a node's resources/capabilities (for its
// manifest) and live CPU/memory metrics (for heartbeats), wrapping
// gopsutil so the rest of the agent never depends on it directly — the
// core domain model has no OS-specific assumptions (v1.md §2).
package sysinfo

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"home-harness/internal/domain"
)

// Manifest collects the static resources and capabilities to publish in a
// node's manifest at registration time.
func Manifest(ctx context.Context) ([]domain.Resource, []domain.Capability, error) {
	cores, err := cpu.CountsWithContext(ctx, true)
	if err != nil {
		return nil, nil, fmt.Errorf("sysinfo: cpu count: %w", err)
	}

	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sysinfo: virtual memory: %w", err)
	}

	usage, err := disk.UsageWithContext(ctx, rootPath())
	if err != nil {
		return nil, nil, fmt.Errorf("sysinfo: disk usage: %w", err)
	}

	resources := []domain.Resource{
		{Kind: domain.ResourceCPUCores, Capacity: float64(cores), Unit: "cores"},
		{Kind: domain.ResourceMemoryBytes, Capacity: float64(vm.Total), Unit: "bytes"},
		{Kind: domain.ResourceStorageBytes, Capacity: float64(usage.Total), Unit: "bytes"},
	}

	// v4 declares the two capabilities the agent actually implements
	// (internal/agent/executor.go's startExecute/startFilesystemRead) —
	// capability-first modeling (baseline §8 rule 2) means a node is
	// described by what it actually exposes, and the manager now enforces
	// this at placement time (internal/manager/placement.go), so declaring
	// a capability here is a real, checked claim, not an aspiration. One
	// exception: an agent run with -insecure still declares both (its
	// hardware/OS capabilities haven't changed) but then refuses every
	// dispatched workload outright (workloads.go's InsecureWorkloadsDisabled
	// check) since it can't verify the manager's identity — a deliberate,
	// tested trade-off (TestInsecureAgentRefusesWorkload), not a bug here.
	return resources, []domain.Capability{
		{Name: domain.CapabilitySystemExecute, Version: "1"},
		{Name: domain.CapabilityFilesystemRead, Version: "1"},
	}, nil
}

// Metrics is the live figures collected on each heartbeat.
type Metrics struct {
	CPUPercent float64
	// CPUScope is "" when CPUPercent is the whole device's, or
	// CPUScopeProcess when the system's CPU counters can't be read and it
	// is this agent's own use instead (see CollectMetrics).
	CPUScope             string
	MemoryAvailableBytes uint64
}

// CPUScopeProcess: the CPU figure is this agent's own (it and the tasks
// it runs in process), not the whole device's.
const CPUScopeProcess = "process"

// CollectMetrics samples current CPU usage (blocking for interval) and
// available memory.
//
// Android doesn't let apps read the system's CPU counters (/proc/stat is
// denied), which would make a worker phone report 0% whatever it does.
// There the figure is the agent's own CPU use instead (Metrics.CPUScope):
// the typed tasks it runs in process, i.e. exactly the work it does for
// the harness.
func CollectMetrics(ctx context.Context, interval time.Duration) (Metrics, error) {
	var cpuPercent float64
	scope := ""
	if systemCPUReadable() {
		percents, err := cpu.PercentWithContext(ctx, interval, false)
		if err != nil {
			return Metrics{}, fmt.Errorf("sysinfo: cpu percent: %w", err)
		}
		if len(percents) > 0 {
			cpuPercent = percents[0]
		}
	} else {
		pct, err := ownCPU.percent(ctx, interval)
		if err != nil {
			return Metrics{}, fmt.Errorf("sysinfo: own cpu use: %w", err)
		}
		cpuPercent, scope = pct, CPUScopeProcess
	}

	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return Metrics{}, fmt.Errorf("sysinfo: virtual memory: %w", err)
	}

	return Metrics{CPUPercent: cpuPercent, CPUScope: scope, MemoryAvailableBytes: vm.Available}, nil
}

// systemCPUReadable reports whether this process may read the system-wide
// CPU counters (not as an Android app).
var systemCPUReadable = sync.OnceValue(func() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	f, err := os.Open("/proc/stat")
	if err != nil {
		return false
	}
	f.Close()
	return true
})

// processCPU measures this process's CPU use between calls: over the time
// since the previous sample (a heartbeat interval, for a steady figure),
// or over interval on the first call.
type processCPU struct {
	mu       sync.Mutex
	lastCPU  float64 // user+system seconds
	lastWall time.Time
}

var ownCPU processCPU

func (p *processCPU) percent(ctx context.Context, interval time.Duration) (float64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	self, err := process.NewProcessWithContext(ctx, int32(os.Getpid()))
	if err != nil {
		return 0, err
	}
	read := func() (float64, error) {
		t, err := self.TimesWithContext(ctx)
		if err != nil {
			return 0, err
		}
		return t.User + t.System, nil
	}
	if p.lastWall.IsZero() {
		if p.lastCPU, err = read(); err != nil {
			return 0, err
		}
		p.lastWall = time.Now()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(interval):
		}
	}
	now, err := read()
	if err != nil {
		return 0, err
	}
	wall := time.Now()
	elapsed := wall.Sub(p.lastWall).Seconds()
	used := now - p.lastCPU
	p.lastCPU, p.lastWall = now, wall
	if elapsed <= 0 {
		return 0, nil
	}
	pct := used / elapsed / float64(runtime.NumCPU()) * 100
	return min(max(pct, 0), 100), nil
}

func rootPath() string {
	if runtime.GOOS == "windows" {
		return `C:\`
	}
	return "/"
}
