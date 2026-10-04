//go:build !windows && !linux && !darwin

package sysinfo

import (
	"context"

	"home-harness/internal/domain"
)

func platformGPUs(context.Context) []domain.GPU { return nil }
