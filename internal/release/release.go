// Package release writes and checks a build's signed release manifest
// (roadmap item 19): SHA256SUMS, every file of a bundle in sha256sum's
// own format (so `sha256sum -c SHA256SUMS` reads it too), and
// SHA256SUMS.sig, an Ed25519 signature over its exact bytes made with the
// builder's release key (~/.home-harness/release-signing.key, kept
// outside the repo like the Android app's signing key).
//
// What it protects: a bundle copied between machines or left on a disk
// can't have a file changed, or one added to what the manager serves,
// without the check failing. What it doesn't: it is not Authenticode, so
// Windows SmartScreen still warns about the programs (only a real
// code-signing certificate changes that). And a check whose key comes
// from the bundle itself proves only that the bundle is intact; the key
// is the real anchor when it comes from outside the bundle — pinned by an
// earlier install, or given by hand.
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// ManifestName lists the bundle's files and their SHA-256.
	ManifestName = "SHA256SUMS"
	// SignatureName holds the release key's signature over the manifest.
	SignatureName = "SHA256SUMS.sig"
	// PublicKeyName is the release public key a build puts in the bundle,
	// for a first install to pin. Not covered by the manifest: it is what
	// the manifest is checked with.
	PublicKeyName = "release-key.pub"
)

// sigAlgorithm opens a signature file's single line.
const sigAlgorithm = "ed25519"

// Entry is one manifest line: a file (slash-separated, relative to the
// manifest's directory) and its lowercase hex SHA-256.
type Entry struct {
	Path   string
	SHA256 string
}

// own reports whether a top-level name is one of the release files
// themselves, which the manifest never lists.
func own(name string) bool {
	return name == ManifestName || name == SignatureName || name == PublicKeyName
}

// BuildManifest hashes every file under dir — except the release files at
// its top — into a manifest, sorted by path. Anything but a regular file
// or a directory (a symlink, say) is refused: what is listed must be what
// gets copied.
func BuildManifest(dir string) ([]byte, error) {
	var entries []Entry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: not a regular file", p)
		case own(rel):
			return nil
		}
		if err := validName(rel); err != nil {
			return err
		}
		sum, err := hashFile(p)
		if err != nil {
			return err
		}
		entries = append(entries, Entry{Path: rel, SHA256: sum})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no files to list in %s", dir)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "%s  %s\n", e.SHA256, e.Path)
	}
	return b.Bytes(), nil
}

// ParseManifest reads a manifest strictly — exactly the shape
// BuildManifest writes: "<64 lowercase hex>  <path>" lines ending in LF,
// paths relative and slash-separated with no "." or ".." parts, no
// backslash or colon (so no drive letter or alternate data stream), and
// no name twice, compared case-insensitively as Windows would.
func ParseManifest(data []byte) ([]Entry, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, errors.New("manifest is empty or doesn't end with a newline")
	}
	var entries []Entry
	seen := make(map[string]bool)
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || !isHexSHA256(sum) {
			return nil, fmt.Errorf("manifest line %d: want \"<sha256>  <path>\"", i+1)
		}
		if err := validName(name); err != nil {
			return nil, fmt.Errorf("manifest line %d: %w", i+1, err)
		}
		if own(name) {
			return nil, fmt.Errorf("manifest line %d: lists %s itself", i+1, name)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, fmt.Errorf("manifest line %d: %s is listed twice", i+1, name)
		}
		seen[key] = true
		entries = append(entries, Entry{Path: name, SHA256: sum})
	}
	return entries, nil
}

func validName(name string) error {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, `\:`) {
		return fmt.Errorf("unsafe file name %q", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("unsafe file name %q", name)
		}
	}
	return nil
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// Sign returns the signature file for manifest: one line naming the
// algorithm, the signing key's ID (for error messages only — a verifier
// never takes the key from it) and the base64 signature.
func Sign(priv ed25519.PrivateKey, manifest []byte) []byte {
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, manifest)
	return []byte(fmt.Sprintf("%s %s %s\n", sigAlgorithm, KeyID(pub), base64.StdEncoding.EncodeToString(sig)))
}

// Verify checks a signature file over manifest with pub.
func Verify(pub ed25519.PublicKey, manifest, sigFile []byte) error {
	fields := strings.Fields(string(sigFile))
	if len(fields) != 3 || fields[0] != sigAlgorithm {
		return errors.New("the signature file is not an ed25519 release signature")
	}
	sig, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("the signature file holds no valid signature")
	}
	if ed25519.Verify(pub, manifest, sig) {
		return nil
	}
	if fields[1] != KeyID(pub) {
		return fmt.Errorf("signed by release key %s, not the expected %s", fields[1], KeyID(pub))
	}
	return errors.New("the signature doesn't match: the manifest was changed after it was signed")
}

// CheckManifest verifies the manifest at path (its signature beside it,
// in SignatureName) with pub, then that every file it lists is present
// under the manifest's directory with the listed hash. It returns the
// entries; a caller decides which files must be among them.
func CheckManifest(path string, pub ed25519.PublicKey) ([]Entry, error) {
	manifest, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sigFile, err := os.ReadFile(filepath.Join(filepath.Dir(path), SignatureName))
	if err != nil {
		return nil, fmt.Errorf("%s has no signature: %w", path, err)
	}
	if err := Verify(pub, manifest, sigFile); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	entries, err := ParseManifest(manifest)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e.Path))
		sum, err := hashFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s is listed in the release manifest but can't be read: %w", e.Path, err)
		}
		if sum != e.SHA256 {
			return nil, fmt.Errorf("%s was changed after the release was built (its SHA-256 doesn't match the signed manifest)", e.Path)
		}
	}
	return entries, nil
}

// Unlisted returns the files under dir (slash-separated, relative) that
// entries doesn't list, leaving out the release files at its top.
func Unlisted(dir string, entries []Entry) ([]string, error) {
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		listed[e.Path] = true
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !own(rel) && !listed[rel] {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// KeyID is a short name for a release key: the first 8 bytes of its
// SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// EncodePublicKey is the text form of a public key (base64 of its 32
// bytes): what PublicKeyName holds and what -release-key takes.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// ParsePublicKey reads EncodePublicKey's form — strictly, so one key has
// one spelling (the last character's unused bits must be zero).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// LoadPublicKey takes a public key given either as its text or as a file
// holding it (a bundle's release-key.pub).
func LoadPublicKey(value string) (ed25519.PublicKey, error) {
	if pub, err := ParsePublicKey(value); err == nil {
		return pub, nil
	}
	data, err := os.ReadFile(value)
	if err != nil {
		return nil, fmt.Errorf("release key %q is neither a base64 Ed25519 public key nor a file holding one", value)
	}
	pub, err := ParsePublicKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("release key file %s: %w", value, err)
	}
	return pub, nil
}

// LoadOrCreateKey reads the release signing key (PKCS#8 PEM) at path, or
// creates one there, owner-only, reporting created so the caller can tell
// the builder to back it up: installs pin its public half, and bundles
// signed by any other key are refused there.
func LoadOrCreateKey(path string) (priv ed25519.PrivateKey, created bool, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "PRIVATE KEY" {
			return nil, false, fmt.Errorf("release key %s: not a PEM private key", path)
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("release key %s: %w", path, err)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, false, fmt.Errorf("release key %s: not an Ed25519 key", path)
		}
		return priv, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("read release key: %w", err)
	}
	_, priv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create release key dir: %w", err)
	}
	// O_EXCL: two builds racing to create it must not each sign with a
	// key the other overwrote.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("create release key: %w", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		return nil, false, err
	}
	return priv, true, f.Close()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
