package fsperm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The access list must name the current user and nobody else, and must not
// inherit from the folder. icacls shows what Windows really enforces.
func TestPrivateLeavesOnlyTheOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Private(p); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("icacls", p).CombinedOutput()
	if err != nil {
		t.Skipf("icacls unavailable: %v", err)
	}
	text := string(out)
	if strings.Contains(text, "(I)") {
		t.Errorf("file still inherits access from its folder:\n%s", text)
	}
	for _, other := range []string{"BUILTIN\\Users", "Everyone", "Authenticated Users"} {
		if strings.Contains(text, other) {
			t.Errorf("%s still has access:\n%s", other, text)
		}
	}
	if strings.Count(text, "(F)") != 1 {
		t.Errorf("want exactly one full-control entry (the owner):\n%s", text)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "x" {
		t.Errorf("owner can no longer read the file: %v", err)
	}
}
