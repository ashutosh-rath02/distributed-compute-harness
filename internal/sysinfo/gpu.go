package sysinfo

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// GPUs lists the device's graphics processors, found once per process
// (they don't change while it runs). Nothing found is an empty list, not
// an error: GPU awareness only ever ranks devices, never rules one out.
func GPUs(ctx context.Context) []domain.GPU {
	gpuOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		gpuList = platformGPUs(ctx)
	})
	return gpuList
}

var (
	gpuOnce sync.Once
	gpuList []domain.GPU
)

// nvidiaSMI runs nvidia-smi from the first of paths that exists — never
// a bare name, which would need a PATH lookup (that crashes the agent on
// Android, and NVIDIA's tools live in fixed places anyway).
func nvidiaSMI(ctx context.Context, paths ...string) []domain.GPU {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, p, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
		if err != nil {
			return nil
		}
		return parseNvidiaSMI(string(out))
	}
	return nil
}

// parseNvidiaSMI reads "name, MiB" lines.
func parseNvidiaSMI(out string) []domain.GPU {
	var gpus []domain.GPU
	for _, line := range strings.Split(out, "\n") {
		name, mem, ok := strings.Cut(strings.TrimSpace(line), ",")
		if !ok {
			continue
		}
		mib, err := strconv.ParseUint(strings.TrimSpace(mem), 10, 64)
		if err != nil {
			continue
		}
		gpus = append(gpus, domain.GPU{Name: strings.TrimSpace(name), Vendor: "nvidia", MemoryBytes: mib << 20})
	}
	return gpus
}

// pciVendor maps a PCI vendor ID ("10de", "0x1002", "VEN_8086") to a name.
func pciVendor(id string) string {
	id = strings.ToLower(id)
	switch {
	case strings.Contains(id, "10de"):
		return "nvidia"
	case strings.Contains(id, "1002"):
		return "amd"
	case strings.Contains(id, "8086"):
		return "intel"
	}
	return ""
}

var (
	intelDiscrete = regexp.MustCompile(`(?i)\barc(\(tm\))?\s+[ab]\d`) // Arc A770, B580 — not Meteor Lake's "Arc Graphics"
	amdDiscrete   = regexp.MustCompile(`(?i)\b(rx|pro|instinct|firepro)\b|radeon\s+(vii|r9|r7)`)
)

// integratedGPU tells laptop and desktop-chip graphics, which share
// system memory, from cards with their own: Intel's are integrated
// except Arc A/B cards, AMD's unless named like a card (RX, Pro, ...).
func integratedGPU(vendor, name string) bool {
	switch vendor {
	case "intel":
		return !intelDiscrete.MatchString(name)
	case "amd":
		return !amdDiscrete.MatchString(name)
	case "apple":
		return true
	}
	return false
}

// drmGPUs reads the GPUs under root (Linux's /sys/class/drm), one per
// cardN. Plain file reading, so it is tested on every OS.
func drmGPUs(root string, haveNvidia bool) []domain.GPU {
	cards, _ := filepath.Glob(filepath.Join(root, "card[0-9]*"))
	var gpus []domain.GPU
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue // a connector (card0-HDMI-A-1), not a device
		}
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join(card, "device", name))
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(b))
		}
		vendor := pciVendor(read("vendor"))
		if vendor == "" || (vendor == "nvidia" && haveNvidia) {
			continue
		}
		name := read("product_name")
		if name == "" {
			name = map[string]string{"nvidia": "NVIDIA GPU", "amd": "AMD GPU", "intel": "Intel GPU"}[vendor]
		}
		g := domain.GPU{Name: name, Vendor: vendor}
		if v, err := strconv.ParseUint(read("mem_info_vram_total"), 10, 64); err == nil {
			g.MemoryBytes = v
		}
		switch vendor {
		case "intel":
			// Arc cards are 0x56xx (Alchemist) and 0xe20x (Battlemage).
			dev := strings.ToLower(read("device"))
			g.Integrated = !strings.HasPrefix(dev, "0x56") && !strings.HasPrefix(dev, "0xe20")
		case "amd":
			// APUs carve a small share of system memory as "VRAM".
			g.Integrated = g.MemoryBytes < 2<<30
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// memorySizeValue reads the binary form of Windows' HardwareInformation.MemorySize
// (a little-endian 4- or 8-byte number).
func memorySizeValue(b []byte) uint64 {
	switch {
	case len(b) >= 8:
		return binary.LittleEndian.Uint64(b)
	case len(b) >= 4:
		return uint64(binary.LittleEndian.Uint32(b))
	}
	return 0
}
