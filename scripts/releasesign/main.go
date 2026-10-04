// Command releasesign writes and checks a bundle's signed release
// manifest (internal/release) for the build scripts:
//
//	go run ./scripts/releasesign sign -dir dist/windows-manager
//	    -> SHA256SUMS (every file), SHA256SUMS.sig, release-key.pub
//	go run ./scripts/releasesign verify -dir dist/windows-manager [-pub KEY]
//	go run ./scripts/releasesign pubkey      (prints the public key)
//
// THE KEY lives outside the repo, created on first use:
//
//	$HARNESS_RELEASE_KEY   (default ~/.home-harness/release-signing.key)
//
// Back it up with the Android signing key next to it. A PC that installed
// a signed bundle pins this key and refuses bundles signed by another, so
// after losing it every such PC needs its pinned key deleted by hand.
//
// The manifest protects a bundle in transit and at rest. It does not make
// Windows trust the programs: SmartScreen warnings only go away with a
// real code-signing certificate (scripts/sign-windows.ps1).
package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"home-harness/internal/release"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "releasesign: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: releasesign sign|verify|pubkey [flags]")
	}
	fl := flag.NewFlagSet("releasesign "+args[0], flag.ContinueOnError)
	fl.SetOutput(stderr)
	dir := fl.String("dir", "", "the bundle directory")
	keyPath := fl.String("key", defaultKeyPath(), "the release signing key (created on first use); default $HARNESS_RELEASE_KEY or ~/.home-harness/release-signing.key")
	pubValue := fl.String("pub", "", "verify: the public key to check with (base64, or a file holding it); default the bundle's own "+release.PublicKeyName+", which proves only that the bundle is intact")
	if err := fl.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "sign":
		if *dir == "" {
			return errors.New("sign: -dir is required")
		}
		priv, err := loadKey(*keyPath, stderr)
		if err != nil {
			return err
		}
		return sign(*dir, priv, stdout)
	case "verify":
		if *dir == "" {
			return errors.New("verify: -dir is required")
		}
		return verify(*dir, *pubValue, stdout, stderr)
	case "pubkey":
		priv, err := loadKey(*keyPath, stderr)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, release.EncodePublicKey(priv.Public().(ed25519.PublicKey)))
		return nil
	}
	return fmt.Errorf("unknown command %q (want sign, verify or pubkey)", args[0])
}

func defaultKeyPath() string {
	if p := os.Getenv("HARNESS_RELEASE_KEY"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".home-harness", "release-signing.key")
}

// loadKey loads the signing key, or creates it and says so loudly: the
// builder must back it up before shipping anything signed with it.
func loadKey(path string, stderr io.Writer) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("no release key path: set HARNESS_RELEASE_KEY")
	}
	priv, created, err := release.LoadOrCreateKey(path)
	if err != nil {
		return nil, err
	}
	if created {
		id := release.KeyID(priv.Public().(ed25519.PublicKey))
		fmt.Fprintf(stderr, "!! created the release signing key %s (key %s).\n", path, id)
		fmt.Fprintln(stderr, "!! BACK IT UP, with the Android signing key next to it: every PC that installs a bundle")
		fmt.Fprintln(stderr, "!! signed with it pins it, and refuses bundles signed with any other key from then on.")
	}
	return priv, nil
}

// sign writes the manifest of everything in dir, its signature, and the
// public key, replacing any from an earlier run.
func sign(dir string, priv ed25519.PrivateKey, stdout io.Writer) error {
	for _, name := range []string{release.ManifestName, release.SignatureName, release.PublicKeyName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	manifest, err := release.BuildManifest(dir)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	files := map[string][]byte{
		release.ManifestName:  manifest,
		release.SignatureName: release.Sign(priv, manifest),
		release.PublicKeyName: []byte(release.EncodePublicKey(pub) + "\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "signed %s: %d files, release key %s\n", filepath.Join(dir, release.ManifestName), strings.Count(string(manifest), "\n"), release.KeyID(pub))
	return nil
}

// verify checks dir's manifest and that it lists every file in dir.
func verify(dir, pubValue string, stdout, stderr io.Writer) error {
	if pubValue == "" {
		pubValue = filepath.Join(dir, release.PublicKeyName)
		fmt.Fprintf(stderr, "note: checking with the bundle's own key, which proves only that the bundle is intact; pass -pub with a key you got elsewhere to check who built it\n")
	}
	pub, err := release.LoadPublicKey(pubValue)
	if err != nil {
		return err
	}
	entries, err := release.CheckManifest(filepath.Join(dir, release.ManifestName), pub)
	if err != nil {
		return err
	}
	extra, err := release.Unlisted(dir, entries)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return fmt.Errorf("not part of the signed release: %s", strings.Join(extra, ", "))
	}
	fmt.Fprintf(stdout, "ok: %d files match the manifest signed by release key %s\n", len(entries), release.KeyID(pub))
	return nil
}
