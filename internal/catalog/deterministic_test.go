package catalog

import (
	"slices"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

// The deterministic set is pinned: marking a type makes the manager
// re-run its results on other devices and flag a device whose bytes
// differ, so a wrong mark accuses honest devices. Each one is backed by
// a run-twice test in internal/tasks (determinism_test.go).
func TestDeterministicTypesArePinned(t *testing.T) {
	var got []domain.CapabilityName
	for _, ty := range Types() {
		if !ty.Deterministic {
			continue
		}
		got = append(got, ty.Name)
		if strings.HasPrefix(string(ty.Name), "llm.") {
			t.Errorf("%s: a model's answer is never byte-identical twice", ty.Name)
		}
		if len(ty.Outputs) == 0 {
			t.Errorf("%s: deterministic but has no output files to compare", ty.Name)
		}
		if ty.Streams || ty.TargetRequired || ty.Internal {
			t.Errorf("%s: deterministic types must be plain batch tasks", ty.Name)
		}
	}
	want := []domain.CapabilityName{"file.hash", "image.resize", "image.stack", "text.count"}
	if !slices.Equal(got, want) {
		t.Fatalf("deterministic types = %v, want %v", got, want)
	}
	// Each of these was looked at and is not, whatever a run-twice test on
	// one machine says: archive.zip stamps entries with the time, and
	// render.fractal's floating point differs between arm64 and amd64.
	for _, name := range []domain.CapabilityName{"archive.zip", "render.fractal", "system.identity", "cpu.burn"} {
		if mustType(t, name).Deterministic {
			t.Errorf("%s must not be marked deterministic", name)
		}
	}
}
