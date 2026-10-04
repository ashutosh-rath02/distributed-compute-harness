package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"home-harness/internal/domain"
)

// container.run (roadmap item 13): a container image run by the device's
// own Docker or Podman. Unlike the other types it runs a program the
// catalog doesn't define, so it is OptIn (off until policy turns it on)
// and its policy also says which images may run (manager/container.go).
// This file holds what both sides need: image references in their
// canonical form, the allow-list rules, and the argv check.

// ContainerRun is the container task type's name.
const ContainerRun domain.CapabilityName = "container.run"

// Capability attributes a container-capable agent advertises: its engine
// ("docker 27.1.1") and the platform its containers run as
// ("linux/amd64"), which a task's platform parameter must match.
const (
	AttrEngine   = "engine"
	AttrPlatform = "platform"
)

// Bounds on a container's argv (the args parameter).
const (
	MaxContainerArgs     = 64
	MaxContainerArgBytes = 8 << 10
	MaxContainerArgv     = 16 << 10 // the whole JSON list
)

// Image reference grammar (Docker's distribution/reference, without IPv6
// hosts and with sha256 digests only):
// [registry[:port]/]path[:tag][@sha256:<64 hex>].
const (
	imageDomainComponent = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
	imageDomain          = imageDomainComponent + `(?:\.` + imageDomainComponent + `)*(?::[0-9]{1,5})?`
	imagePathComponent   = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	imagePath            = imagePathComponent + `(?:/` + imagePathComponent + `)*`
	imageTag             = `[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}`
	imageDigest          = `sha256:[a-f0-9]{64}`
)

// ImagePattern matches a container image reference.
const ImagePattern = `(?:` + imageDomain + `/)?` + imagePath + `(?::` + imageTag + `)?(?:@` + imageDigest + `)?`

// PlatformPattern matches a container platform, os/arch ("linux/arm64").
const PlatformPattern = `[a-z0-9]+/[a-z0-9_]+`

var (
	imageRe       = regexp.MustCompile(`^(?:` + ImagePattern + `)$`)
	imageDomainRe = regexp.MustCompile(`^` + imageDomain + `$`)
	imagePathRe   = regexp.MustCompile(`^` + imagePath + `$`)
	imageTagRe    = regexp.MustCompile(`^` + imageTag + `$`)
)

// Image is a parsed image reference in canonical form: the registry is
// explicit ("docker.io/library/alpine", never "alpine", so Podman never
// guesses a registry and the allow-list compares like with like) and a
// digest, when given, replaces the tag (the engine ignores a tag next to
// a digest).
type Image struct {
	Repo   string // registry/path, e.g. docker.io/library/alpine
	Tag    string // "" when pinned by Digest
	Digest string // sha256:<hex>, or ""
}

func (i Image) String() string {
	if i.Digest != "" {
		return i.Repo + "@" + i.Digest
	}
	return i.Repo + ":" + i.Tag
}

// Pinned reports whether the image is named by digest (its content can't
// change under the same name, unlike a tag).
func (i Image) Pinned() bool { return i.Digest != "" }

// ParseImage parses and canonicalizes an image reference; a reference
// with neither tag nor digest means its "latest" tag, as for the engines.
func ParseImage(ref string) (Image, error) {
	img, err := parseRef(ref)
	if err != nil {
		return Image{}, err
	}
	if img.Tag == "" && img.Digest == "" {
		img.Tag = "latest"
	}
	return img, nil
}

func parseRef(ref string) (Image, error) {
	if !imageRe.MatchString(ref) {
		return Image{}, fmt.Errorf("%q is not an image reference ([registry/]name[:tag][@sha256:...])", ref)
	}
	rest, digest, _ := strings.Cut(ref, "@")
	tag := ""
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		rest, tag = rest[:i], rest[i+1:]
	}
	domainPart, path := "docker.io", rest
	if first, after, ok := strings.Cut(rest, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost" || first != strings.ToLower(first)) {
		domainPart, path = strings.ToLower(first), after
	}
	if domainPart == "index.docker.io" {
		domainPart = "docker.io"
	}
	if domainPart == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if !imageDomainRe.MatchString(domainPart) || !imagePathRe.MatchString(path) || (tag != "" && !imageTagRe.MatchString(tag)) {
		return Image{}, fmt.Errorf("%q is not an image reference ([registry/]name[:tag][@sha256:...])", ref)
	}
	if digest != "" {
		tag = ""
	}
	return Image{Repo: domainPart + "/" + path, Tag: tag, Digest: digest}, nil
}

// canonImage is the image parameter's canonical form.
func canonImage(v string) (string, error) {
	img, err := ParseImage(v)
	if err != nil {
		return "", err
	}
	return img.String(), nil
}

// CanonAllowedImage checks one allowed-images entry and returns its
// canonical form. An entry is a repository ("docker.io/library/alpine":
// any tag or digest of it), one tag or one digest of it
// ("...alpine:3.20", "...alpine@sha256:..."), or every repository under a
// registry or namespace ("docker.io/library/*", "ghcr.io/me/*"). A bare
// "*" is refused: the list is meant to name what may run.
func CanonAllowedImage(entry string) (string, error) {
	entry = strings.TrimSpace(entry)
	if prefix, ok := strings.CutSuffix(entry, "/*"); ok {
		p, err := canonImagePrefix(prefix)
		if err != nil {
			return "", fmt.Errorf("allowed image %q: %v", entry, err)
		}
		return p + "/*", nil
	}
	img, err := parseRef(entry)
	if err != nil {
		return "", fmt.Errorf("allowed image %q: use a repository (docker.io/library/alpine), a digest (...@sha256:...) or a namespace (docker.io/library/*)", entry)
	}
	if img.Tag == "" && img.Digest == "" {
		return img.Repo, nil
	}
	return img.String(), nil
}

// canonImagePrefix canonicalizes the part of a wildcard entry before
// "/*": a registry alone ("docker.io", "ghcr.io"), or a registry and
// namespace ("ghcr.io/me"), or a Docker Hub namespace ("library").
func canonImagePrefix(p string) (string, error) {
	if p == "" || strings.Contains(p, "*") {
		return "", errors.New(`name a registry or namespace before "/*", e.g. docker.io/library/*`)
	}
	first, rest, hasRest := strings.Cut(p, "/")
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		if !imageDomainRe.MatchString(first) || (hasRest && !imagePathRe.MatchString(rest)) {
			return "", errors.New("not a registry or namespace")
		}
		first = strings.ToLower(first)
		if first == "index.docker.io" {
			first = "docker.io"
		}
		if !hasRest {
			return first, nil
		}
		return first + "/" + rest, nil
	}
	if !imagePathRe.MatchString(p) {
		return "", errors.New("not a registry or namespace")
	}
	return "docker.io/" + p, nil
}

// ImageAllowed reports whether img (canonical) matches an allowed-images
// list (canonical entries). An empty list allows nothing.
func ImageAllowed(img Image, allowed []string) bool {
	for _, e := range allowed {
		if prefix, ok := strings.CutSuffix(e, "/*"); ok {
			// The slash keeps "docker.io/library/*" from matching
			// "docker.io/library-evil/x".
			if strings.HasPrefix(img.Repo, prefix+"/") {
				return true
			}
			continue
		}
		rule, err := parseRef(e)
		if err != nil || rule.Repo != img.Repo {
			continue
		}
		if (rule.Digest == "" || rule.Digest == img.Digest) && (rule.Tag == "" || rule.Tag == img.Tag) {
			return true
		}
	}
	return false
}

// canonArgv checks the args parameter, a container's command line as a
// JSON list of strings, and returns it re-encoded compactly. The list
// goes after the image, so it is only ever the container's own argv,
// never options to the engine: elements may start with '-'.
func canonArgv(v string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(v), "[") {
		return "", errors.New(`must be a JSON list of strings, e.g. ["echo","hello"]`)
	}
	// Elements as raw JSON first: decoding straight into []string would
	// turn a null into "".
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return "", errors.New(`must be a JSON list of strings, e.g. ["echo","hello"]`)
	}
	if len(raw) > MaxContainerArgs {
		return "", fmt.Errorf("at most %d arguments", MaxContainerArgs)
	}
	args := make([]string, len(raw))
	for i, a := range raw {
		if len(a) == 0 || a[0] != '"' || json.Unmarshal(a, &args[i]) != nil {
			return "", errors.New(`must be a JSON list of strings, e.g. ["echo","hello"]`)
		}
	}
	for i, a := range args {
		if len(a) > MaxContainerArgBytes {
			return "", fmt.Errorf("argument %d is longer than %d bytes", i+1, MaxContainerArgBytes)
		}
		for _, r := range a {
			if (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f {
				return "", fmt.Errorf("argument %d contains a control character", i+1)
			}
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return "", err
	}
	out := strings.TrimSuffix(b.String(), "\n")
	if len(out) > MaxContainerArgv {
		return "", fmt.Errorf("longer than %d bytes", MaxContainerArgv)
	}
	return out, nil
}

// ContainerArgs decodes a canonical args parameter ("" = none: the
// image's own command).
func ContainerArgs(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	var args []string
	if err := json.Unmarshal([]byte(v), &args); err != nil {
		return nil, err
	}
	return args, nil
}
