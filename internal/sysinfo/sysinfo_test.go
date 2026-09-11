package sysinfo

import (
	"context"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func TestManifestReturnsPlausibleResources(t *testing.T) {
	resources, capabilities, err := Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	kinds := map[domain.ResourceKind]float64{}
	for _, r := range resources {
		kinds[r.Kind] = r.Capacity
	}

	if kinds[domain.ResourceCPUCores] <= 0 {
		t.Fatalf("expected positive CPU core count, got %v", kinds[domain.ResourceCPUCores])
	}
	if kinds[domain.ResourceMemoryBytes] <= 0 {
		t.Fatalf("expected positive memory total, got %v", kinds[domain.ResourceMemoryBytes])
	}
	if kinds[domain.ResourceStorageBytes] <= 0 {
		t.Fatalf("expected positive storage total, got %v", kinds[domain.ResourceStorageBytes])
	}
	if len(capabilities) == 0 {
		t.Fatal("expected at least one declared capability")
	}
}

func TestCollectMetricsReturnsPlausibleValues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m, err := CollectMetrics(ctx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	if m.CPUPercent < 0 || m.CPUPercent > 100 {
		t.Fatalf("expected CPU percent in [0,100], got %v", m.CPUPercent)
	}
	if m.MemoryAvailableBytes == 0 {
		t.Fatal("expected non-zero available memory")
	}
}
