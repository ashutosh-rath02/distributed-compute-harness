package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"home-harness/internal/manager"
	"home-harness/internal/release"
)

// releaseKey is the release public key (base64) built into this manager:
// scripts/build-windows-manager.sh sets it with
// -ldflags "-X main.releaseKey=...", and it is -release-key's default. A
// plain go build has none. With a key, -check-agent-binaries requires a
// signed -release-manifest — so a manager built with one can't vet an
// unsigned agent set.
var releaseKey string

// checkRelease is -check-agent-binaries' release check (roadmap item 19):
// with a release key, the bundle's manifest must carry that key's
// signature, every file it lists must match, and every file this manager
// would serve — each agent build and the app — must be listed. With
// neither a key nor a manifest it checks nothing, as before. It returns a
// line describing what it checked.
func checkRelease(key, manifestPath string, agents []manager.AgentBinary, appAPK string) (string, error) {
	switch {
	case key == "" && manifestPath == "":
		return "", nil
	case key == "":
		return "", errors.New("-release-manifest needs -release-key: the key the release must be signed with")
	case manifestPath == "":
		return "", errors.New("a release key is set (-release-key, or built in): give -release-manifest, the bundle's " + release.ManifestName + ", so the builds are checked against it")
	}
	pub, err := release.LoadPublicKey(key)
	if err != nil {
		return "", err
	}
	entries, err := release.CheckManifest(manifestPath, pub)
	if err != nil {
		return "", err
	}
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		listed[e.Path] = true
	}
	dir, err := filepath.Abs(filepath.Dir(manifestPath))
	if err != nil {
		return "", err
	}
	served := make([]string, 0, len(agents)+1)
	for _, a := range agents {
		served = append(served, a.Path)
	}
	if appAPK != "" {
		served = append(served, appAPK)
	}
	for _, p := range served {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(dir, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s is outside the release (%s)", p, dir)
		}
		if !listed[filepath.ToSlash(rel)] {
			return "", fmt.Errorf("%s is not part of the signed release: it isn't in %s", p, manifestPath)
		}
	}
	return fmt.Sprintf("release: %d files match %s, signed by release key %s", len(entries), manifestPath, release.KeyID(pub)), nil
}
