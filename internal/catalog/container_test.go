package catalog

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestImageReferencesAreCanonical(t *testing.T) {
	for ref, want := range map[string]string{
		"alpine":                             "docker.io/library/alpine:latest",
		"alpine:3.20":                        "docker.io/library/alpine:3.20",
		"alpine@" + testDigest:               "docker.io/library/alpine@" + testDigest,
		"alpine:3.20@" + testDigest:          "docker.io/library/alpine@" + testDigest, // the engine ignores the tag
		"library/alpine":                     "docker.io/library/alpine:latest",
		"me/tool:v1":                         "docker.io/me/tool:v1",
		"index.docker.io/me/tool":            "docker.io/me/tool:latest",
		"docker.io/library/alpine":           "docker.io/library/alpine:latest",
		"ghcr.io/me/tool@" + testDigest:      "ghcr.io/me/tool@" + testDigest,
		"localhost:5000/tool":                "localhost:5000/tool:latest",
		"Registry.Example.COM:443/a/b-c_d.e": "registry.example.com:443/a/b-c_d.e:latest",
		"foo:5000":                           "docker.io/library/foo:5000",
	} {
		got, err := canonImage(ref)
		if err != nil || got != want {
			t.Errorf("canonImage(%q) = %q, %v; want %q", ref, got, err, want)
			continue
		}
		if again, err := canonImage(got); err != nil || again != got {
			t.Errorf("not a fixed point: %q -> %q (%v)", got, again, err)
		}
	}
	for _, bad := range []string{"", "-alpine", "--privileged", "Alpine", "alpine:", "a b", "alpine@sha256:abc", "alpine@md5:" + strings.Repeat("0", 32),
		"/alpine", "alpine/", "alpine;rm", "alpine\n", "docker.io/../x", "a..b"} {
		if got, err := canonImage(bad); err == nil {
			t.Errorf("canonImage(%q) = %q, want an error", bad, got)
		}
	}
}

func TestAllowedImagesMatchTheirScopeOnly(t *testing.T) {
	canon := func(entries ...string) []string {
		var out []string
		for _, e := range entries {
			c, err := CanonAllowedImage(e)
			if err != nil {
				t.Fatalf("CanonAllowedImage(%q): %v", e, err)
			}
			out = append(out, c)
		}
		return out
	}
	if got := canon("alpine", "library/*", "docker.io/*", "ghcr.io/me/*", "alpine:3.20", "alpine@"+testDigest, "localhost:5000/*"); !slices.Equal(got, []string{
		"docker.io/library/alpine", "docker.io/library/*", "docker.io/*", "ghcr.io/me/*", "docker.io/library/alpine:3.20", "docker.io/library/alpine@" + testDigest, "localhost:5000/*"}) {
		t.Fatalf("canonical entries %q", got)
	}
	for _, bad := range []string{"*", "/*", "*/*", "docker.io/*/x", "Alpine", "a b/*", "-x"} {
		if c, err := CanonAllowedImage(bad); err == nil {
			t.Errorf("CanonAllowedImage(%q) = %q, want an error", bad, c)
		}
	}
	img := func(ref string) Image {
		i, err := ParseImage(ref)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	cases := []struct {
		allowed []string
		ref     string
		want    bool
	}{
		{nil, "alpine@" + testDigest, false},
		{canon("docker.io/library/*"), "alpine@" + testDigest, true},
		{canon("docker.io/library/*"), "docker.io/library-evil/x@" + testDigest, false},
		{canon("docker.io/library/*"), "docker.io/libraryx/tool@" + testDigest, false},
		{canon("docker.io/library/*"), "docker.io/libraryx@" + testDigest, true}, // an official image: docker.io/library/libraryx
		{canon("ghcr.io/me/*"), "ghcr.io/me/tool/sub@" + testDigest, true},
		{canon("ghcr.io/me/*"), "ghcr.io/mean/tool@" + testDigest, false},
		{canon("ghcr.io/me/*"), "ghcr.io.evil.com/me/tool@" + testDigest, false},
		{canon("alpine"), "alpine@" + testDigest, true},
		{canon("alpine"), "alpine:3.20", true},
		{canon("alpine"), "alpinex@" + testDigest, false},
		{canon("alpine@" + testDigest), "alpine@" + testDigest, true},
		{canon("alpine@" + testDigest), "alpine@sha256:" + strings.Repeat("f", 64), false},
		{canon("alpine@" + testDigest), "alpine:latest", false},
		{canon("alpine:3.20"), "alpine:3.20", true},
		{canon("alpine:3.20"), "alpine:3.21", false},
		{canon("alpine:3.20"), "alpine:3.20@sha256:" + strings.Repeat("f", 64), false}, // a digest is not the tag
		{canon("docker.io/*"), "ghcr.io/me/tool@" + testDigest, false},
	}
	for _, c := range cases {
		if got := ImageAllowed(img(c.ref), c.allowed); got != c.want {
			t.Errorf("ImageAllowed(%s, %q) = %v", c.ref, c.allowed, got)
		}
	}
}

func TestContainerArgvIsAJSONListOfPlainStrings(t *testing.T) {
	for in, want := range map[string]string{
		`["echo", "hi"]`:                 `["echo","hi"]`,
		`[]`:                             `[]`,
		` ["--privileged","-v","/:/h"] `: `["--privileged","-v","/:/h"]`, // the container's own argv: dashes are fine
		`["sh","-c","a <b> && c\n\td"]`:  `["sh","-c","a <b> && c\n\td"]`,
		`["caf` + `\` + `u00e9"]`:        `["café"]`,
	} {
		got, err := canonArgv(in)
		if err != nil || got != want {
			t.Errorf("canonArgv(%s) = %s, %v; want %s", in, got, err, want)
			continue
		}
		if again, err := canonArgv(got); err != nil || again != got {
			t.Errorf("not a fixed point: %s -> %s", got, again)
		}
	}
	many := "[" + strings.Repeat(`"a",`, MaxContainerArgs) + `"a"]`
	for _, bad := range []string{"", "echo hi", `"echo"`, `null`, `{"a":1}`, `[1,2]`, `["a",null]`, `["a\u0000b"]`, `["a\rb"]`, `["\u001b[31m"]`, many,
		`["` + strings.Repeat("x", MaxContainerArgBytes+1) + `"]`, `["a"`} {
		if got, err := canonArgv(bad); err == nil {
			t.Errorf("canonArgv(%.40q) = %.40q, want an error", bad, got)
		}
	}
	args, err := ContainerArgs(`["sh","-c","x"]`)
	if err != nil || !slices.Equal(args, []string{"sh", "-c", "x"}) {
		t.Fatalf("ContainerArgs: %q %v", args, err)
	}
	if args, err := ContainerArgs(""); err != nil || args != nil {
		t.Fatalf("no args: %q %v", args, err)
	}
}

func TestContainerRunCompiles(t *testing.T) {
	ty := mustType(t, ContainerRun)
	if !ty.OptIn {
		t.Fatal("container.run must be opt-in")
	}
	params, outputs, err := ty.Compile(map[string]string{"image": "alpine@" + testDigest, "args": `[ "cat", "/in/a.txt" ]`, "outputs": "r.txt,logs/run.log"}, img("a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"image": "docker.io/library/alpine@" + testDigest, "args": `["cat","/in/a.txt"]`, "outputs": "r.txt,logs/run.log",
		"cpus": "1", "memory_mb": "512", "network": "none", "timeout": "600"}
	if len(params) != len(want) {
		t.Fatalf("params %v", params)
	}
	for k, v := range want {
		if params[k] != v {
			t.Errorf("%s = %q, want %q", k, params[k], v)
		}
	}
	if !slices.Equal(outputs, []string{"r.txt", "logs/run.log"}) {
		t.Fatalf("outputs %q", outputs)
	}
	// What the agent receives compiles to itself.
	again, outs, err := ty.Compile(params, img("a.txt"))
	if err != nil || !slices.Equal(outs, outputs) || len(again) != len(params) {
		t.Fatalf("recompile: %v %v %q", err, again, outs)
	}
	for k, v := range params {
		if again[k] != v {
			t.Fatalf("recompiled %s = %q, want %q", k, again[k], v)
		}
	}
	// No outputs is fine: stdout is the result.
	if _, outs, err := ty.Compile(map[string]string{"image": "alpine@" + testDigest}, nil); err != nil || len(outs) != 0 {
		t.Fatalf("no outputs: %v %q", err, outs)
	}
	many := strings.TrimSuffix(strings.Repeat("f.txt,", 17), ",")
	for name, p := range map[string]map[string]string{
		"no image":         {},
		"bad image":        {"image": "-x"},
		"bad args":         {"image": "alpine", "args": "rm -rf /"},
		"host network":     {"image": "alpine", "network": "host"},
		"bad output":       {"image": "alpine", "outputs": "../x"},
		"output dot":       {"image": "alpine", "outputs": "a/./b"},
		"too many outputs": {"image": "alpine", "outputs": many},
		"bad platform":     {"image": "alpine", "platform": "linux"},
		"cpus":             {"image": "alpine", "cpus": "0"},
		"memory":           {"image": "alpine", "memory_mb": "1"},
		"unknown":          {"image": "alpine", "privileged": "true"},
	} {
		if _, _, err := ty.Compile(p, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPlatformMatchesOnlyWhenAsked(t *testing.T) {
	ty := mustType(t, ContainerRun)
	amd := map[string]string{AttrPlatform: "linux/amd64"}
	if ok, _ := ty.Offers(amd, map[string]string{"image": "x"}); !ok {
		t.Fatal("no platform asked: any node")
	}
	if ok, _ := ty.Offers(amd, map[string]string{"platform": "linux/amd64"}); !ok {
		t.Fatal("same platform")
	}
	if ok, why := ty.Offers(amd, map[string]string{"platform": "linux/arm64"}); ok || !strings.Contains(why, "linux/amd64") {
		t.Fatalf("other platform: %v %s", ok, why)
	}
	if ok, _ := ty.Offers(nil, map[string]string{"platform": "linux/arm64"}); ok {
		t.Fatal("a node advertising no platform")
	}
	if !ty.HasChoices() {
		t.Fatal("platform is node-specific")
	}
}
