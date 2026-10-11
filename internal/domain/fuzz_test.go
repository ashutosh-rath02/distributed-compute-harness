package domain

import (
	"path"
	"strings"
	"testing"
)

// FuzzValidArtifactName: a name that passes can never leave the working
// directory or name a device, on any platform.
func FuzzValidArtifactName(f *testing.F) {
	for _, s := range []string{"a.txt", "dir/a.txt", "../a", "a/../b", "nul.txt", "a.", "a\\b", "C:/x", "a b", "", "/a", "a//b", strings.Repeat("a", 65)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if ValidArtifactName(name) != nil {
			return
		}
		if len(name) > 128 || path.Clean(name) != name || strings.ContainsAny(name, "\\:*?\"<>| \x00") || strings.HasPrefix(name, "/") {
			t.Fatalf("accepted %q", name)
		}
		for _, seg := range strings.Split(name, "/") {
			if seg == ".." || seg == "." || seg == "" || strings.HasSuffix(seg, ".") {
				t.Fatalf("accepted %q", name)
			}
		}
	})
}
