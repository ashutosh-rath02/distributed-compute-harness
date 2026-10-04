//go:build !windows && !linux && !darwin

package sysinfo

import (
	"context"

	"home-harness/internal/domain"
)

func platformDeviceUse(context.Context) domain.DeviceUse { return domain.DeviceUse{} }
