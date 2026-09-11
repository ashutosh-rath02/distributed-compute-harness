// Package sysinfo collects a node's resources/capabilities (for its
// manifest) and live CPU/memory metrics (for heartbeats), wrapping
// gopsutil so the rest of the agent never depends on it directly — the
// core domain model has no OS-specific assumptions (v1.md §2).
package sysinfo

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

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
	CPUPercent           float64
	MemoryAvailableBytes uint64
}

// CollectMetrics samples current CPU usage (blocking for interval) and
// available memory.
func CollectMetrics(ctx context.Context, interval time.Duration) (Metrics, error) {
	percents, err := cpu.PercentWithContext(ctx, interval, false)
	if err != nil {
		return Metrics{}, fmt.Errorf("sysinfo: cpu percent: %w", err)
	}
	var cpuPercent float64
	if len(percents) > 0 {
		cpuPercent = percents[0]
	}

	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return Metrics{}, fmt.Errorf("sysinfo: virtual memory: %w", err)
	}

	return Metrics{CPUPercent: cpuPercent, MemoryAvailableBytes: vm.Available}, nil
}

func rootPath() string {
	if runtime.GOOS == "windows" {
		return `C:\`
	}
	return "/"
}
