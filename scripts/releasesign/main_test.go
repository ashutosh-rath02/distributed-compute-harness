package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"home-harness/internal/release"
)

func bundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"manager.exe": "m", "agents/agent-linux-arm64": "a", "install.ps1": "i"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSignCreatesTheKeyOnceAndVerifies(t *testing.T) {
	dir := bundle(t)
	key := filepath.Join(t.TempDir(), "home", "release-signing.key")
	var out, errs bytes.Buffer
	if err := run([]string{"sign", "-dir", dir, "-key", key}, &out, &errs); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.Contains(errs.String(), "BACK IT UP") {
		t.Fatalf("creating the key wasn't announced: %q", errs.String())
	}
	for _, name := range []string{release.ManifestName, release.SignatureName, release.PublicKeyName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("sign didn't write %s: %v", name, err)
		}
	}
	// Signing again (a rebuild) reuses the key quietly and gives the same
	// manifest.
	first, _ := os.ReadFile(filepath.Join(dir, release.ManifestName))
	errs.Reset()
	if err := run([]string{"sign", "-dir", dir, "-key", key}, &out, &errs); err != nil {
		t.Fatalf("second sign: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, release.ManifestName))
	if errs.Len() != 0 || !bytes.Equal(first, second) {
		t.Fatalf("second sign: notice %q, manifest changed %v", errs.String(), !bytes.Equal(first, second))
	}

	// pubkey prints what the bundle carries.
	out.Reset()
	if err := run([]string{"pubkey", "-key", key}, &out, &errs); err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	pubFile, _ := os.ReadFile(filepath.Join(dir, release.PublicKeyName))
	if out.String() != string(pubFile) {
		t.Fatalf("pubkey = %q, bundle has %q", out.String(), pubFile)
	}

	out.Reset()
	if err := run([]string{"verify", "-dir", dir, "-pub", strings.TrimSpace(string(pubFile))}, &out, &errs); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out.String(), "ok: 3 files") {
		t.Fatalf("verify said %q", out.String())
	}
}

func TestVerifyRejectsAChangedOrAddedFile(t *testing.T) {
	for name, tamper := range map[string]func(dir string){
		"changed": func(dir string) {
			os.WriteFile(filepath.Join(dir, "agents", "agent-linux-arm64"), []byte("evil"), 0o644)
		},
		"added": func(dir string) {
			os.WriteFile(filepath.Join(dir, "agents", "agent-darwin-arm64"), []byte("evil"), 0o644)
		},
		"unsigned": func(dir string) {
			os.Remove(filepath.Join(dir, release.SignatureName))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := bundle(t)
			key := filepath.Join(t.TempDir(), "release-signing.key")
			var out, errs bytes.Buffer
			if err := run([]string{"sign", "-dir", dir, "-key", key}, &out, &errs); err != nil {
				t.Fatalf("sign: %v", err)
			}
			tamper(dir)
			if err := run([]string{"verify", "-dir", dir}, &out, &errs); err == nil {
				t.Fatalf("verify passed a bundle with a file %s", name)
			}
		})
	}
}

func TestDefaultKeyPathHonoursTheEnvironment(t *testing.T) {
	t.Setenv("HARNESS_RELEASE_KEY", filepath.Join("x", "k.key"))
	if got := defaultKeyPath(); got != filepath.Join("x", "k.key") {
		t.Fatalf("defaultKeyPath = %q", got)
	}
	t.Setenv("HARNESS_RELEASE_KEY", "")
	if got := defaultKeyPath(); !strings.HasSuffix(got, filepath.Join(".home-harness", "release-signing.key")) {
		t.Fatalf("defaultKeyPath = %q", got)
	}
}
