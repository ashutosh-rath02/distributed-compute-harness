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

// capabilityVersion is a placeholder version for the illustrative
// capabilities declared below — none of them are actually invocable yet
// (see command.go's CommandName set for what v0 really supports).
const capabilityVersion = "0.1"

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

	// v1.md §5's example manifest lists these as illustrative capabilities
	// even though v0 has no runtime capable of invoking them — the point
	// is to prove the schema can carry declarations for functionality that
	// arrives later without redesigning Manifest itself.
	capabilities := []domain.Capability{
		{Name: domain.CapabilitySystemExecute, Version: capabilityVersion},
		{Name: domain.CapabilityFilesystemRead, Version: capabilityVersion},
		{Name: domain.CapabilityFilesystemWrite, Version: capabilityVersion},
	}

	return resources, capabilities, nil
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
