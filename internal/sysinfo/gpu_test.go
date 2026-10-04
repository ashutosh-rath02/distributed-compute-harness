package sysinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNvidiaSMI(t *testing.T) {
	gpus := parseNvidiaSMI("NVIDIA GeForce RTX 3060, 12288\nNVIDIA GeForce GTX 1050 Ti, 4096\n\ngarbage\n")
	if len(gpus) != 2 || gpus[0].Name != "NVIDIA GeForce RTX 3060" || gpus[0].MemoryBytes != 12<<30 || gpus[0].Integrated || gpus[1].MemoryBytes != 4<<30 {
		t.Fatalf("%+v", gpus)
	}
}

func TestIntegratedGPU(t *testing.T) {
	for name, want := range map[string]bool{
		"intel|Intel(R) Iris(R) Xe Graphics":   true,
		"intel|Intel(R) UHD Graphics 620":      true,
		"intel|Intel(R) Arc(TM) Graphics":      true, // Meteor Lake's built-in
		"intel|Intel(R) Arc(TM) A770 Graphics": false,
		"intel|Intel(R) Arc(TM) B580 Graphics": false,
		"amd|AMD Radeon(TM) Graphics":          true, // Ryzen APU
		"amd|AMD Radeon(TM) 780M":              true,
		"amd|AMD Radeon RX 7800 XT":            false,
		"amd|AMD Radeon Pro W6800":             false,
		"nvidia|NVIDIA GeForce RTX 4090":       false,
		"apple|Apple silicon GPU":              true,
	} {
		vendor, gpu := split(name)
		if got := integratedGPU(vendor, gpu); got != want {
			t.Errorf("%s: integrated=%v, want %v", name, got, want)
		}
	}
}

func split(s string) (string, string) {
	for i := range s {
		if s[i] == '|' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

func TestPCIVendorAndMemorySize(t *testing.T) {
	for id, want := range map[string]string{"PCI\\VEN_8086&DEV_46A6": "intel", "0x10de": "nvidia", "0x1002": "amd", "PCI\\VEN_1414&DEV_008C": ""} {
		if got := pciVendor(id); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	// This laptop's Iris Xe: a 4-byte binary 0x7FFFF000.
	if got := memorySizeValue([]byte{0, 240, 255, 127}); got != 2147479552 {
		t.Errorf("4 bytes: %d", got)
	}
	if got := memorySizeValue([]byte{0, 0, 0, 0, 3, 0, 0, 0}); got != 12<<30 {
		t.Errorf("8 bytes: %d", got)
	}
}

func TestDRMGPUs(t *testing.T) {
	root := t.TempDir()
	write := func(card, name, content string) {
		p := filepath.Join(root, card, "device", name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(content+"\n"), 0o600)
	}
	write("card0", "vendor", "0x8086")
	write("card0", "device", "0x46a6")
	write("card1", "vendor", "0x1002")
	write("card1", "mem_info_vram_total", "17163091968")
	write("card1", "product_name", "Radeon RX 7800 XT")
	write("card2", "vendor", "0x10de")
	// A connector, not a device: on Linux its device link leads to the
	// card's files, so it must not count as a second GPU.
	write("card1-DP-1", "vendor", "0x1002")
	write("card1-DP-1", "mem_info_vram_total", "17163091968")
	gpus := drmGPUs(root, true) // NVIDIA already listed by nvidia-smi
	if len(gpus) != 2 || gpus[0].Vendor != "intel" || !gpus[0].Integrated || gpus[1].Name != "Radeon RX 7800 XT" || gpus[1].Integrated || gpus[1].MemoryBytes != 17163091968 {
		t.Fatalf("%+v", gpus)
	}
	if gpus := drmGPUs(filepath.Join(root, "missing"), false); len(gpus) != 0 {
		t.Fatalf("no drm: %+v", gpus)
	}
}
