package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"home-harness/internal/manager"
	"home-harness/internal/release"
)

// releaseBundle lays out a signed bundle like dist/windows-manager and
// returns its directory, manifest path and the signing key's text.
func releaseBundle(t *testing.T) (dir, manifestPath, pubText string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	files := map[string]string{
		"manager.exe":                    "manager",
		"home-harness.apk":               "app",
		"agents/agent-windows-amd64.exe": "windows agent",
		"agents/agent-linux-arm64":       "arm64 agent",
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := release.BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath = filepath.Join(dir, release.ManifestName)
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, release.SignatureName), release.Sign(priv, manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, manifestPath, release.EncodePublicKey(pub)
}

func agentsIn(dir string, names ...string) []manager.AgentBinary {
	var out []manager.AgentBinary
	for _, n := range names {
		out = append(out, manager.AgentBinary{Path: filepath.Join(dir, "agents", n)})
	}
	return out
}

func TestCheckReleaseAcceptsTheSignedBundle(t *testing.T) {
	dir, manifestPath, key := releaseBundle(t)
	agents := agentsIn(dir, "agent-windows-amd64.exe", "agent-linux-arm64")
	got, err := checkRelease(key, manifestPath, agents, filepath.Join(dir, "home-harness.apk"))
	if err != nil {
		t.Fatalf("checkRelease: %v", err)
	}
	if !strings.Contains(got, "4 files match") {
		t.Fatalf("checkRelease said %q", got)
	}
	// The key as a file (a bundle's release-key.pub) works the same, and
	// so does an explicit os/arch=path build.
	keyFile := filepath.Join(t.TempDir(), release.PublicKeyName)
	os.WriteFile(keyFile, []byte(key+"\n"), 0o644)
	agents[0].OS, agents[0].Arch = "windows", "amd64"
	if _, err := checkRelease(keyFile, manifestPath, agents, ""); err != nil {
		t.Fatalf("checkRelease with a key file: %v", err)
	}
}

func TestCheckReleaseWithoutAKeyChecksNothing(t *testing.T) {
	// Exactly as before this check existed: an unsigned agent set passes.
	got, err := checkRelease("", "", agentsIn(t.TempDir(), "agent.exe"), "")
	if err != nil || got != "" {
		t.Fatalf("checkRelease without a key = %q, %v", got, err)
	}
}

func TestCheckReleaseRefuses(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, dir, manifestPath, key string) error
		want string
	}{
		{"a key but no manifest (a built-in key can't be skipped)", func(t *testing.T, dir, manifestPath, key string) error {
			_, err := checkRelease(key, "", agentsIn(dir, "agent-linux-arm64"), "")
			return err
		}, "give -release-manifest"},
		{"a manifest but no key", func(t *testing.T, dir, manifestPath, key string) error {
			_, err := checkRelease("", manifestPath, agentsIn(dir, "agent-linux-arm64"), "")
			return err
		}, "needs -release-key"},
		{"an agent build added to the bundle", func(t *testing.T, dir, manifestPath, key string) error {
			os.WriteFile(filepath.Join(dir, "agents", "agent-darwin-arm64"), []byte("evil"), 0o644)
			_, err := checkRelease(key, manifestPath, agentsIn(dir, "agent-linux-arm64", "agent-darwin-arm64"), "")
			return err
		}, "agent-darwin-arm64 is not part of the signed release"},
		{"an app added outside the manifest", func(t *testing.T, dir, manifestPath, key string) error {
			os.WriteFile(filepath.Join(dir, "other.apk"), []byte("evil"), 0o644)
			_, err := checkRelease(key, manifestPath, agentsIn(dir, "agent-linux-arm64"), filepath.Join(dir, "other.apk"))
			return err
		}, "other.apk is not part of the signed release"},
		{"an agent build from outside the bundle", func(t *testing.T, dir, manifestPath, key string) error {
			outside := filepath.Join(t.TempDir(), "agent-linux-arm64")
			os.WriteFile(outside, []byte("arm64 agent"), 0o644)
			_, err := checkRelease(key, manifestPath, []manager.AgentBinary{{Path: outside}}, "")
			return err
		}, "is outside the release"},
		{"a changed agent build", func(t *testing.T, dir, manifestPath, key string) error {
			os.WriteFile(filepath.Join(dir, "agents", "agent-linux-arm64"), []byte("evil"), 0o644)
			_, err := checkRelease(key, manifestPath, agentsIn(dir, "agent-linux-arm64"), "")
			return err
		}, "agents/agent-linux-arm64 was changed"},
		{"a changed file the manager doesn't serve", func(t *testing.T, dir, manifestPath, key string) error {
			os.WriteFile(filepath.Join(dir, "manager.exe"), []byte("evil"), 0o644)
			_, err := checkRelease(key, manifestPath, agentsIn(dir, "agent-linux-arm64"), "")
			return err
		}, "manager.exe was changed"},
		{"another release key", func(t *testing.T, dir, manifestPath, key string) error {
			_, _, other := releaseBundle(t)
			_, err := checkRelease(other, manifestPath, agentsIn(dir, "agent-linux-arm64"), "")
			return err
		}, "signed by release key"},
		{"a key that isn't one", func(t *testing.T, dir, manifestPath, key string) error {
			_, err := checkRelease("not-a-key", manifestPath, agentsIn(dir, "agent-linux-arm64"), "")
			return err
		}, "neither a base64 Ed25519 public key nor a file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, manifestPath, key := releaseBundle(t)
			err := tc.run(t, dir, manifestPath, key)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkRelease = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
