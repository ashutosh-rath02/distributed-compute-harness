package manager

import (
	"debug/elf"
	"debug/pe"
)

// detectBinaryPlatform best-effort identifies the OS/architecture a
// binary at path was built for, by reading its file header — stdlib
// debug/pe and debug/elf, no new dependency. Returns ("", "") if the file
// can't be opened or its format isn't one of the two this project
// actually builds for (Windows/amd64 agent.exe, Linux/arm64 for Termux).
//
// This exists so the "(outdated)" marker (cmd/harnessctl's `nodes`
// listing, the web dashboard) can be platform-aware: comparing a Linux
// node's binary hash against a currently-served Windows binary's hash
// always differs, regardless of whether that Linux node is actually
// up to date — without knowing what platform is being served, every
// cross-platform node would incorrectly show as outdated forever. Rather
// than adding an -agent-binary-platform flag (one more thing an operator
// has to remember to set correctly), this reads it directly from the
// file itself.
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
	return "", ""
}
