package manager

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Minimal valid headers for each executable format the catalog detects —
// enough for debug/elf, debug/macho, and debug/pe to parse the machine
// type, without shipping real cross-compiled binaries in the repo.
func elfHeader(machine uint16) []byte {
	h := make([]byte, 64)
	copy(h, []byte{0x7f, 'E', 'L', 'F', 2 /* 64-bit */, 1 /* little-endian */, 1 /* version */})
	binary.LittleEndian.PutUint16(h[16:], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(h[18:], machine)
	binary.LittleEndian.PutUint32(h[20:], 1)  // version
	binary.LittleEndian.PutUint16(h[52:], 64) // ehsize
	return h
}

func machoHeader(cpu uint32) []byte {
	h := make([]byte, 32)
	binary.LittleEndian.PutUint32(h[0:], 0xfeedfacf) // MH_MAGIC_64
	binary.LittleEndian.PutUint32(h[4:], cpu)
	binary.LittleEndian.PutUint32(h[12:], 2) // MH_EXECUTE
	return h
}

func peHeader(machine uint16) []byte {
	// DOS header + PE signature + COFF file header, then padding: debug/pe
	// reads past the COFF header (the empty string table) even when there
	// are no sections or symbols.
	h := make([]byte, 0x40+4+20+64)
	copy(h, "MZ")
	binary.LittleEndian.PutUint32(h[0x3c:], 0x40)
	copy(h[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(h[0x44:], machine)
	return h
}

func writeFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestDetectBinaryPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch string
		content        []byte
	}{
		{"elf-arm64", "linux", "arm64", elfHeader(183)}, // EM_AARCH64
		{"elf-amd64", "linux", "amd64", elfHeader(62)},  // EM_X86_64
		{"macho-arm64", "darwin", "arm64", machoHeader(0x0100000c)},
		{"macho-amd64", "darwin", "amd64", machoHeader(0x01000007)},
		{"pe-amd64", "windows", "amd64", peHeader(0x8664)},
		{"pe-arm64", "windows", "arm64", peHeader(0xaa64)},
		{"garbage", "", "", []byte("not an executable")},
	} {
		goos, arch := detectBinaryPlatform(writeFile(t, tc.name, tc.content))
		if goos != tc.os || arch != tc.arch {
			t.Errorf("%s: detected %s/%s, want %s/%s", tc.name, goos, arch, tc.os, tc.arch)
		}
	}

	// And one genuine, full-size executable: this test binary itself.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if goos, arch := detectBinaryPlatform(self); goos != runtime.GOOS || arch != runtime.GOARCH {
		t.Fatalf("detected the running test binary as %s/%s, want %s/%s", goos, arch, runtime.GOOS, runtime.GOARCH)
	}
}

func TestBuildAgentCatalog(t *testing.T) {
	linux := writeFile(t, "agent-linux-arm64", elfHeader(183))
	windows := writeFile(t, "agent.exe", peHeader(0x8664))
	opaque := writeFile(t, "opaque", []byte("dummy test agent"))

	c, err := BuildAgentCatalog([]AgentBinary{{Path: linux}, {Path: windows}, {OS: "darwin", Arch: "arm64", Path: opaque}})
	if err != nil {
		t.Fatalf("BuildAgentCatalog: %v", err)
	}
	if b, ok := c.forPlatform("linux", "arm64"); !ok || b.Path != linux || b.SHA256 == "" {
		t.Fatalf("expected the detected linux/arm64 build, got %+v %v", b, ok)
	}
	if b, ok := c.forPlatform("darwin", "arm64"); !ok || b.Path != opaque {
		t.Fatalf("expected the explicitly declared darwin/arm64 build, got %+v %v", b, ok)
	}
	if _, ok := c.forPlatform("linux", "amd64"); ok {
		t.Fatal("expected no build for an unloaded platform — the catalog has no wildcard")
	}
	// Linux was listed first, but the legacy route must still serve
	// Windows: every pre-catalog agent in the field is a Windows build.
	if p, _ := c.primary(); p.Path != windows {
		t.Fatalf("expected primary to prefer windows/amd64 regardless of order, got %s", p.platform())
	}
	if views := c.views(); len(views) != 3 || views[0].Path != "/agent-binaries/linux/arm64" {
		t.Fatalf("unexpected catalog views: %+v", views)
	}

	for name, bad := range map[string][]AgentBinary{
		"undetectable without explicit platform": {{Path: opaque}},
		"explicit platform contradicting header": {{OS: "windows", Arch: "amd64", Path: linux}},
		"duplicate platform":                     {{Path: linux}, {OS: "linux", Arch: "arm64", Path: opaque}},
		"half-specified platform":                {{OS: "linux", Path: opaque}},
		"missing file":                           {{Path: filepath.Join(t.TempDir(), "absent")}},
	} {
		if _, err := BuildAgentCatalog(bad); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	if c, err := BuildAgentCatalog(nil); err != nil || !c.empty() {
		t.Fatalf("expected an empty catalog for no binaries, got %v %v", c, err)
	}
	if p, ok := (&agentCatalog{builds: []agentBuild{{OS: "linux", Arch: "arm64"}}}).primary(); !ok || p.OS != "linux" {
		t.Fatalf("expected primary to fall back to the first build without windows/amd64, got %+v", p)
	}
}

func TestParseAgentBinaryFlag(t *testing.T) {
	if b, err := ParseAgentBinaryFlag(`C:\agents\agent.exe`); err != nil || b.Path != `C:\agents\agent.exe` || b.OS != "" {
		t.Fatalf("plain path: got %+v, %v", b, err)
	}
	if b, err := ParseAgentBinaryFlag("linux/arm64=/sdcard/agent"); err != nil || b.OS != "linux" || b.Arch != "arm64" || b.Path != "/sdcard/agent" {
		t.Fatalf("explicit platform: got %+v, %v", b, err)
	}
	for _, bad := range []string{"", "linux=/x", "/arm64=/x", "linux/=/x", "linux/arm64="} {
		if _, err := ParseAgentBinaryFlag(bad); err == nil || !strings.Contains(err.Error(), "agent-binary") {
			t.Errorf("ParseAgentBinaryFlag(%q): expected an error, got %v", bad, err)
		}
	}
}
