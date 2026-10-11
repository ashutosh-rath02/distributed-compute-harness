package version

import (
	"strings"
	"testing"
)

func TestStringIncludesInjectedFields(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, Date
	defer func() { Version, Commit, Date = oldV, oldC, oldD }()
	Version, Commit, Date = "1.2.3", "0123456789abcdef", "2026-10-09T12:00:00Z"

	got := String("agent")
	for _, want := range []string{"agent 1.2.3", "commit 0123456789ab", "built 2026-10-09T12:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "0123456789abc") {
		t.Errorf("commit should be shortened: %q", got)
	}
	info := Get()
	if info.Version != "1.2.3" || info.Commit != Commit || info.Platform == "" {
		t.Errorf("Get() = %+v", info)
	}
}

func TestDefaultIsADevBuild(t *testing.T) {
	if !strings.Contains(Version, "dev") {
		// Only a release build overrides it; tests never do.
		t.Skipf("built with an injected version %q", Version)
	}
	if got := String("manager"); !strings.HasPrefix(got, "manager 0.0.0-dev") {
		t.Errorf("String() = %q", got)
	}
}
