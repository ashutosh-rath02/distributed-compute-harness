package main

import (
	"slices"
	"testing"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

func TestContainerPolicySettings(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var tp domain.TypePolicy
	run := func(args ...string) error { return setContainerPolicy(catalog.ContainerRun, &tp, args[0], args[1:]) }
	for _, args := range [][]string{
		{"images", "add", "alpine@" + digest},
		{"images", "add", "library/*,ghcr.io/me/*"},
		{"images", "add", "docker.io/library/alpine@" + digest}, // already there
		{"tags", "on"},
		{"network", "on"},
	} {
		if err := run(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if want := []string{"docker.io/library/alpine@" + digest, "docker.io/library/*", "ghcr.io/me/*"}; !slices.Equal(tp.AllowImages, want) || !tp.AllowTags || !tp.AllowNetwork {
		t.Fatalf("policy %+v", tp)
	}
	if err := run("images", "remove", "library/*"); err != nil || len(tp.AllowImages) != 2 {
		t.Fatalf("remove: %v %q", err, tp.AllowImages)
	}
	if err := run("images", "remove", "nothing/here"); err == nil {
		t.Fatal("removing an image that isn't there")
	}
	if err := run("images", "set", "alpine"); err != nil || !slices.Equal(tp.AllowImages, []string{"docker.io/library/alpine"}) {
		t.Fatalf("set: %v %q", err, tp.AllowImages)
	}
	if err := run("images", "-"); err != nil || tp.AllowImages != nil {
		t.Fatalf("clear: %v %q", err, tp.AllowImages)
	}
	for _, bad := range [][]string{{"images", "add", "*"}, {"images", "add"}, {"tags", "maybe"}, {"network"}, {"colour", "red"}} {
		if err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := setContainerPolicy("image.resize", &tp, "network", []string{"on"}); err == nil {
		t.Error("network on another type")
	}
}
