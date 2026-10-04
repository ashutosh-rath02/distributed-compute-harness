package manager

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

const (
	testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testImage  = "docker.io/library/alpine@" + testDigest
)

func containerServer(t *testing.T, tp *domain.TypePolicy) *Server {
	t.Helper()
	p := domain.DefaultPolicy()
	if tp != nil {
		p.Types[catalog.ContainerRun] = *tp
	}
	s := NewServer(nil, nil, Config{})
	if _, err := s.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	return s
}

func compiled(t *testing.T, params map[string]string) map[string]string {
	t.Helper()
	ty, _ := catalog.Lookup(catalog.ContainerRun)
	canon, _, err := ty.Compile(params, nil)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// container.run is off until policy turns it on, also under a policy
// stored before it existed (which has no entry for it).
func TestContainerRunIsOffByDefault(t *testing.T) {
	stored := domain.Policy{Types: map[domain.CapabilityName]domain.TypePolicy{domain.CapabilitySystemExecute: {Enabled: true}}}
	for name, p := range map[string]domain.Policy{"default": domain.DefaultPolicy(), "stored before it existed": stored, "empty": {}} {
		if effectivePolicy(p, catalog.ContainerRun).Enabled {
			t.Errorf("%s: container.run on", name)
		}
		if !effectivePolicy(p, "image.resize").Enabled {
			t.Errorf("%s: an ordinary catalog type off", name)
		}
	}
	if !effectivePolicy(domain.PermissivePolicy(), catalog.ContainerRun).Enabled {
		t.Error("the permissive (library) policy allows everything listed")
	}
	s := containerServer(t, nil)
	err := s.checkPolicy(catalog.ContainerRun)
	if !errors.Is(err, ErrPolicy) || !strings.Contains(err.Error(), `"harnessctl policy type container.run on"`) {
		t.Fatalf("off: %v", err)
	}
	if got := effectivePolicy(domain.DefaultPolicy(), catalog.ContainerRun).MaxRuntimeSeconds; got != 3600 {
		t.Fatalf("default max runtime %d", got)
	}
}

func TestContainerPolicyChecksImageTagAndNetwork(t *testing.T) {
	s := containerServer(t, &domain.TypePolicy{Enabled: true, AllowImages: []string{"docker.io/library/*"}})
	req := domain.ResourceRequirements{}
	if err := s.checkContainer(catalog.ContainerRun, compiled(t, map[string]string{"image": "alpine@" + testDigest, "cpus": "2", "memory_mb": "1024"}), &req); err != nil {
		t.Fatal(err)
	}
	// The reservation follows the container's own limits.
	if req.MinMemoryBytes != 1024<<20 || req.MinCPUCores != 2 {
		t.Fatalf("reservation %+v", req)
	}

	for name, c := range map[string]struct {
		params map[string]string
		says   string
	}{
		"not allowed": {map[string]string{"image": "ghcr.io/me/tool@" + testDigest},
			`"harnessctl policy type container.run images add ghcr.io/me/tool@` + testDigest + `"`},
		"lookalike namespace": {map[string]string{"image": "docker.io/library-evil/x@" + testDigest}, "images add"},
		"tag":                 {map[string]string{"image": "alpine:3.20"}, `"harnessctl policy type container.run tags on"`},
		"network":             {map[string]string{"image": "alpine@" + testDigest, "network": "bridge"}, `"harnessctl policy type container.run network on"`},
	} {
		err := s.checkContainer(catalog.ContainerRun, compiled(t, c.params), nil)
		if !errors.Is(err, ErrPolicy) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s = containerServer(t, &domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine"}, AllowTags: true, AllowNetwork: true})
	for _, p := range []map[string]string{{"image": "alpine:3.20"}, {"image": "alpine@" + testDigest, "network": "bridge"}} {
		if err := s.checkContainer(catalog.ContainerRun, compiled(t, p), nil); err != nil {
			t.Errorf("%v: %v", p, err)
		}
	}
	// Other types pass untouched.
	if err := s.checkContainer("image.resize", map[string]string{"image": "whatever"}, &req); err != nil {
		t.Fatal(err)
	}
}

func TestContainerPolicyIsValidatedAndCanonical(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	p := domain.DefaultPolicy()
	p.Types[catalog.ContainerRun] = domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine", " library/* ", "docker.io/library/alpine", "alpine@" + testDigest}}
	if _, err := s.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	got := s.policy.get().Types[catalog.ContainerRun].AllowImages
	if !slices.Equal(got, []string{"docker.io/library/alpine", "docker.io/library/*", testImage}) {
		t.Fatalf("stored %q", got)
	}
	// A copy, not the stored slice.
	got[0] = "changed"
	if s.policy.get().Types[catalog.ContainerRun].AllowImages[0] == "changed" {
		t.Fatal("policy shares its image list")
	}

	many := make([]string, maxAllowedImages+1)
	for i := range many {
		many[i] = "docker.io/me/tool" + strings.Repeat("x", i)
	}
	for _, c := range []struct {
		name domain.CapabilityName
		tp   domain.TypePolicy
	}{
		{"image.resize", domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine"}}},
		{"system.execute", domain.TypePolicy{AllowNetwork: true}},
		{"cpu.burn", domain.TypePolicy{AllowTags: true}},
		{catalog.ContainerRun, domain.TypePolicy{Enabled: true, AllowImages: []string{"*"}}},
		{catalog.ContainerRun, domain.TypePolicy{Enabled: true, AllowImages: []string{"Not An Image"}}},
		{catalog.ContainerRun, domain.TypePolicy{Enabled: true, AllowImages: many}},
	} {
		bad := domain.DefaultPolicy()
		bad.Types[c.name] = c.tp
		if _, err := s.SetPolicy(bad); err == nil {
			t.Errorf("%s %+v accepted", c.name, c.tp)
		}
	}
}

func TestContainerRestartIsRecheckedAgainstPolicy(t *testing.T) {
	s := containerServer(t, &domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine"}})
	w := domain.Workload{Capability: catalog.ContainerRun, Params: compiled(t, map[string]string{"image": "alpine@" + testDigest})}
	if err := s.restartAllowed(w); err != nil {
		t.Fatal(err)
	}
	p := s.policy.get()
	p.Types[catalog.ContainerRun] = domain.TypePolicy{Enabled: true, AllowImages: []string{"ghcr.io/me/*"}}
	s.SetPolicy(p)
	if err := s.restartAllowed(w); !errors.Is(err, ErrPolicy) {
		t.Fatalf("restart after the image was dropped: %v", err)
	}
}

func TestPlannerNeverOffersContainers(t *testing.T) {
	s := containerServer(t, &domain.TypePolicy{Enabled: true, AllowImages: []string{"alpine"}})
	readyNode(t, s.Registry, "n1", 2)
	s.Registry.UpdateResources("n1", nil, []domain.Capability{{Name: "system.identity", Version: "1"}, {Name: catalog.ContainerRun, Version: "1",
		Attributes: map[string]string{catalog.AttrPlatform: "linux/amd64", catalog.AttrEngine: "docker 27.1.1"}}})
	var names []domain.CapabilityName
	for _, pt := range s.plannableTypes() {
		names = append(names, pt.Name)
	}
	if !slices.Contains(names, "system.identity") || slices.Contains(names, catalog.ContainerRun) {
		t.Fatalf("plannable %v", names)
	}
}
