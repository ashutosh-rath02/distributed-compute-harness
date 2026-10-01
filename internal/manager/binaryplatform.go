package manager

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
)

// detectBinaryPlatform best-effort identifies the GOOS/GOARCH a binary at
// path was built for, by reading its file header — stdlib debug/pe,
// debug/elf, and debug/macho, no new dependency. Returns ("", "") if the
// file can't be opened or isn't a recognized executable format, and an
// empty arch for a recognized format on an unrecognized CPU.
//
// It keys the agent catalog (agentcatalog.go), so the platform an operator
// gets is read from the file itself rather than from a flag they could get
// wrong. Android/Termux agents are GOOS=linux builds, so they detect (and
// report themselves) as linux/arm64.
func detectBinaryPlatform(path string) (goos, arch string) {
	if f, err := pe.Open(path); err == nil {
		defer f.Close()
		switch f.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64:
			return "windows", "amd64"
		case pe.IMAGE_FILE_MACHINE_ARM64:
			return "windows", "arm64"
		}
		return "windows", ""
	}
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		switch f.Machine {
		case elf.EM_AARCH64:
			return "linux", "arm64"
		case elf.EM_X86_64:
			return "linux", "amd64"
		}
		return "linux", ""
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		switch f.Cpu {
		case macho.CpuArm64:
			return "darwin", "arm64"
		case macho.CpuAmd64:
			return "darwin", "amd64"
		}
		return "darwin", ""
	}
	return "", ""
}
