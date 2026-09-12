package manager

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// minimalELF64 builds just enough of a valid ELF64 header for debug/elf to
// open successfully and report Machine — no program/section headers
// (counts zeroed), since detectBinaryPlatform only reads the header.
func minimalELF64(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b[0:4], []byte{0x7f, 'E', 'L', 'F'})
	b[4] = 2 // ELFCLASS64
	b[5] = 1 // ELFDATA2LSB (little endian)
	b[6] = 1 // EI_VERSION
	le := binary.LittleEndian
	le.PutUint16(b[16:18], 2)       // e_type = ET_EXEC
	le.PutUint16(b[18:20], machine) // e_machine
	le.PutUint32(b[20:24], 1)       // e_version
	le.PutUint16(b[52:54], 64)      // e_ehsize
	return b
}

// minimalPE builds just enough of a valid PE (DOS header + "PE\0\0" +
// COFF file header, zero sections, zero-size optional header) for
// debug/pe to open successfully and report Machine.
func minimalPE(machine uint16) []byte {
	// debug/pe.NewFile always reads a fixed 96-byte DOS header up front
	// (regardless of e_lfanew), so the file must be at least that long
	// before the PE header even starts.
	dos := make([]byte, 96)
	copy(dos[0:2], []byte{'M', 'Z'})
	binary.LittleEndian.PutUint32(dos[0x3C:0x40], 96) // e_lfanew: PE header right after this

	pe := make([]byte, 4+20)
	copy(pe[0:4], []byte{'P', 'E', 0, 0})
	binary.LittleEndian.PutUint16(pe[4:6], machine) // Machine
	// NumberOfSections, TimeDateStamp, PointerToSymbolTable,
	// NumberOfSymbols, SizeOfOptionalHeader, Characteristics all left 0.
	return append(dos, pe...)
}

func TestDetectBinaryPlatformELFAArch64(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, minimalELF64(0xB7 /* EM_AARCH64 */), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	goos, arch := detectBinaryPlatform(path)
	if goos != "linux" || arch != "arm64" {
		t.Fatalf("detectBinaryPlatform = (%q, %q), want (linux, arm64)", goos, arch)
	}
}

func TestDetectBinaryPlatformPEAMD64(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.exe")
	if err := os.WriteFile(path, minimalPE(0x8664 /* IMAGE_FILE_MACHINE_AMD64 */), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	goos, arch := detectBinaryPlatform(path)
	if goos != "windows" || arch != "amd64" {
		t.Fatalf("detectBinaryPlatform = (%q, %q), want (windows, amd64)", goos, arch)
	}
}

func TestDetectBinaryPlatformUnrecognizedFormatReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-binary")
	if err := os.WriteFile(path, []byte("just some text"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	goos, arch := detectBinaryPlatform(path)
	if goos != "" || arch != "" {
		t.Fatalf("detectBinaryPlatform = (%q, %q), want (\"\", \"\") for an unrecognized file", goos, arch)
	}
}

func TestDetectBinaryPlatformMissingFileReturnsEmpty(t *testing.T) {
	goos, arch := detectBinaryPlatform(filepath.Join(t.TempDir(), "does-not-exist"))
	if goos != "" || arch != "" {
		t.Fatalf("detectBinaryPlatform = (%q, %q), want (\"\", \"\") for a missing file", goos, arch)
	}
}
