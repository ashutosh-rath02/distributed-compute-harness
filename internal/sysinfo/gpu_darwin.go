package sysinfo

import (
	"context"
	"runtime"

	"home-harness/internal/domain"
)

// platformGPUs: Apple silicon's GPU shares the system's memory (Ollama
// runs models on it through Metal, but there is no separate VRAM to fit
// a model in). Intel Macs: not reported.
func platformGPUs(context.Context) []domain.GPU {
	if runtime.GOARCH == "arm64" {
		return []domain.GPU{{Name: "Apple silicon GPU", Vendor: "apple", Integrated: true}}
	}
	return nil
}
