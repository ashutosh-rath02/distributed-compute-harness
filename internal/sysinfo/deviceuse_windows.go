package sysinfo

import (
	"context"
	"unsafe"

	"golang.org/x/sys/windows"

	"home-harness/internal/domain"
)

var (
	modKernel32              = windows.NewLazySystemDLL("kernel32.dll")
	modUser32                = windows.NewLazySystemDLL("user32.dll")
	procGetSystemPowerStatus = modKernel32.NewProc("GetSystemPowerStatus")
	procGetTickCount         = modKernel32.NewProc("GetTickCount")
	procGetLastInputInfo     = modUser32.NewProc("GetLastInputInfo")
)

// systemPowerStatus is SYSTEM_POWER_STATUS.
type systemPowerStatus struct {
	ACLineStatus        byte // 0 offline, 1 online, 255 unknown
	BatteryFlag         byte // 128: no system battery; 255: unknown
	BatteryLifePercent  byte // 255: unknown
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// platformDeviceUse: power from GetSystemPowerStatus; idle time from
// GetLastInputInfo, which sees the input of this process's session — the
// signed-in user's, for an agent run as their scheduled task (a service in
// session 0 would always look idle).
func platformDeviceUse(context.Context) domain.DeviceUse {
	var u domain.DeviceUse
	var st systemPowerStatus
	if ok, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&st))); ok != 0 {
		u = powerFromStatus(st)
	}
	var lii struct{ Size, Time uint32 }
	lii.Size = uint32(unsafe.Sizeof(lii))
	if ok, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii))); ok != 0 {
		now, _, _ := procGetTickCount.Call()
		// uint32 arithmetic survives the tick count's 49-day wrap.
		idle := int64(uint32(now)-lii.Time) / 1000
		u.IdleSeconds = &idle
	}
	return u
}

func powerFromStatus(st systemPowerStatus) domain.DeviceUse {
	var u domain.DeviceUse
	switch {
	case st.BatteryFlag == 128: // no battery: always on mains
		f := false
		u.OnBattery = &f
	case st.BatteryFlag == 255 || st.ACLineStatus == 255:
		// unknown
	default:
		onBattery := st.ACLineStatus == 0
		u.OnBattery = &onBattery
		if st.BatteryLifePercent <= 100 {
			p := int(st.BatteryLifePercent)
			u.BatteryPercent = &p
		}
	}
	return u
}
