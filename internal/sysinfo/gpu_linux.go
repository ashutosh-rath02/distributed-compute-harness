package sysinfo

import (
	"context"

	"home-harness/internal/domain"
)

// platformGPUs: NVIDIA cards from nvidia-smi when installed, others from
// /sys/class/drm (AMD's amdgpu reports its VRAM there). Phones have no
// GPU Ollama can use and usually expose nothing here.
func platformGPUs(ctx context.Context) []domain.GPU {
	gpus := nvidiaSMI(ctx, "/usr/bin/nvidia-smi", "/usr/local/bin/nvidia-smi", "/usr/lib/wsl/lib/nvidia-smi")
	return append(gpus, drmGPUs("/sys/class/drm", len(gpus) > 0)...)
}
