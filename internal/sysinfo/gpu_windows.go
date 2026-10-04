package sysinfo

import (
	"context"
	"regexp"

	"golang.org/x/sys/windows/registry"

	"home-harness/internal/domain"
)

// The display adapters' class key: one numbered subkey per installed
// display driver, readable without admin rights.
const displayClassKey = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

var fourDigits = regexp.MustCompile(`^\d{4}$`)

// platformGPUs: NVIDIA cards from nvidia-smi when it is installed (exact
// memory), everything else from the display drivers' registry entries.
func platformGPUs(ctx context.Context) []domain.GPU {
	nvidia := nvidiaSMI(ctx, `C:\Windows\System32\nvidia-smi.exe`, `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`)
	var gpus []domain.GPU
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nvidia
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	for _, n := range names {
		if !fourDigits.MatchString(n) {
			continue
		}
		sub, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		g, ok := gpuFromDriverKey(sub)
		sub.Close()
		if !ok || (g.Vendor == "nvidia" && len(nvidia) > 0) {
			continue
		}
		gpus = append(gpus, g)
	}
	return append(nvidia, gpus...)
}

func gpuFromDriverKey(k registry.Key) (domain.GPU, bool) {
	name, _, err := k.GetStringValue("DriverDesc")
	if err != nil || name == "" {
		return domain.GPU{}, false
	}
	id, _, _ := k.GetStringValue("MatchingDeviceId")
	vendor := pciVendor(id)
	if vendor == "" {
		return domain.GPU{}, false // virtual or remote-desktop adapters
	}
	g := domain.GPU{Name: name, Vendor: vendor, Integrated: integratedGPU(vendor, name)}
	if v, _, err := k.GetIntegerValue("HardwareInformation.qwMemorySize"); err == nil {
		g.MemoryBytes = v
	} else if b, _, err := k.GetBinaryValue("HardwareInformation.MemorySize"); err == nil {
		g.MemoryBytes = memorySizeValue(b)
	} else if v, _, err := k.GetIntegerValue("HardwareInformation.MemorySize"); err == nil {
		g.MemoryBytes = v
	}
	return g, true
}
