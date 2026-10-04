package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// signedBundle writes a small bundle, its manifest and signature, the way
// scripts/releasesign does, and returns the manifest path.
func signedBundle(t *testing.T, priv ed25519.PrivateKey) (dir, manifestPath string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "manager.exe"), "manager")
	writeFile(t, filepath.Join(dir, "agents", "agent-linux-arm64"), "agent arm64")
	writeFile(t, filepath.Join(dir, "agents", "agent-windows-amd64.exe"), "agent windows")
	writeFile(t, filepath.Join(dir, PublicKeyName), EncodePublicKey(priv.Public().(ed25519.PublicKey))+"\n")
	manifest, err := BuildManifest(dir)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	manifestPath = filepath.Join(dir, ManifestName)
	writeFile(t, manifestPath, string(manifest))
	writeFile(t, filepath.Join(dir, SignatureName), string(Sign(priv, manifest)))
	return dir, manifestPath
}

func TestBuildManifestIsSha256sumFormat(t *testing.T) {
	priv := newKey(t)
	dir, manifestPath := signedBundle(t, priv)
	got, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted, two spaces, slash paths, LF, and none of the release files
	// themselves: exactly what `sha256sum -c` reads.
	want := sum("agent arm64") + "  agents/agent-linux-arm64\n" +
		sum("agent windows") + "  agents/agent-windows-amd64.exe\n" +
		sum("manager") + "  manager.exe\n"
	if string(got) != want {
		t.Fatalf("manifest =\n%s\nwant\n%s", got, want)
	}
	// Building again over the signed bundle gives the same bytes: the
	// release files are never listed.
	again, err := BuildManifest(dir)
	if err != nil || string(again) != want {
		t.Fatalf("rebuilt manifest = %q, %v", again, err)
	}
}

func TestCheckManifestAcceptsTheSignedBundle(t *testing.T) {
	priv := newKey(t)
	dir, manifestPath := signedBundle(t, priv)
	entries, err := CheckManifest(manifestPath, priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("CheckManifest: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %v", entries)
	}
	extra, err := Unlisted(dir, entries)
	if err != nil || len(extra) != 0 {
		t.Fatalf("Unlisted = %v, %v", extra, err)
	}
}

func TestCheckManifestRejectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, dir string)
		want   string
	}{
		{"changed file", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "agents", "agent-linux-arm64"), "evil agent")
		}, "agents/agent-linux-arm64 was changed"},
		{"missing file", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "manager.exe"))
		}, "manager.exe is listed"},
		{"manifest edited to match a changed file", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "manager.exe"), "evil")
			m, _ := os.ReadFile(filepath.Join(dir, ManifestName))
			writeFile(t, filepath.Join(dir, ManifestName), strings.Replace(string(m), sum("manager"), sum("evil"), 1))
		}, "the manifest was changed"},
		{"line added to the manifest", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "extra"), "x")
			m, _ := os.ReadFile(filepath.Join(dir, ManifestName))
			writeFile(t, filepath.Join(dir, ManifestName), string(m)+sum("x")+"  extra\n")
		}, "the manifest was changed"},
		{"signature bit flipped", func(t *testing.T, dir string) {
			s, _ := os.ReadFile(filepath.Join(dir, SignatureName))
			f := strings.Fields(string(s))
			b := []byte(f[2])
			if b[0] == 'A' {
				b[0] = 'B'
			} else {
				b[0] = 'A'
			}
			writeFile(t, filepath.Join(dir, SignatureName), f[0]+" "+f[1]+" "+string(b)+"\n")
		}, "the manifest was changed"},
		{"signature missing", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, SignatureName))
		}, "has no signature"},
		{"signature garbage", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, SignatureName), "ed25519 0011 not-base64!\n")
		}, "no valid signature"},
		{"other algorithm", func(t *testing.T, dir string) {
			s, _ := os.ReadFile(filepath.Join(dir, SignatureName))
			writeFile(t, filepath.Join(dir, SignatureName), strings.Replace(string(s), "ed25519", "rsa", 1))
		}, "not an ed25519 release signature"},
		{"re-signed with another key", func(t *testing.T, dir string) {
			other := newKey(t)
			writeFile(t, filepath.Join(dir, "manager.exe"), "evil")
			m, _ := BuildManifest(dir)
			writeFile(t, filepath.Join(dir, ManifestName), string(m))
			writeFile(t, filepath.Join(dir, SignatureName), string(Sign(other, m)))
		}, "signed by release key "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priv := newKey(t)
			dir, manifestPath := signedBundle(t, priv)
			tc.tamper(t, dir)
			_, err := CheckManifest(manifestPath, priv.Public().(ed25519.PublicKey))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckManifest = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestVerifyNamesTheOtherKey(t *testing.T) {
	signer, expected := newKey(t), newKey(t)
	manifest := []byte(sum("a") + "  a\n")
	err := Verify(expected.Public().(ed25519.PublicKey), manifest, Sign(signer, manifest))
	want := "signed by release key " + KeyID(signer.Public().(ed25519.PublicKey)) + ", not the expected " + KeyID(expected.Public().(ed25519.PublicKey))
	if err == nil || err.Error() != want {
		t.Fatalf("Verify = %v, want %q", err, want)
	}
	// The key ID in the file is only a label: claiming the expected key's
	// ID doesn't make another key's signature pass.
	sig := strings.Replace(string(Sign(signer, manifest)), KeyID(signer.Public().(ed25519.PublicKey)), KeyID(expected.Public().(ed25519.PublicKey)), 1)
	if err := Verify(expected.Public().(ed25519.PublicKey), manifest, []byte(sig)); err == nil {
		t.Fatal("a signature by another key passed by claiming the expected key's ID")
	}
}

func TestUnlistedFindsAddedFiles(t *testing.T) {
	priv := newKey(t)
	dir, manifestPath := signedBundle(t, priv)
	writeFile(t, filepath.Join(dir, "agents", "agent-evil"), "x")
	entries, err := CheckManifest(manifestPath, priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("CheckManifest: %v", err)
	}
	extra, err := Unlisted(dir, entries)
	if err != nil || len(extra) != 1 || extra[0] != "agents/agent-evil" {
		t.Fatalf("Unlisted = %v, %v", extra, err)
	}
}

func TestParseManifestRejectsUnsafeLines(t *testing.T) {
	h := sum("x")
	bad := map[string]string{
		"parent":           h + "  ../manager.exe\n",
		"inner parent":     h + "  agents/../../x\n",
		"dot":              h + "  ./x\n",
		"absolute":         h + "  /etc/passwd\n",
		"drive letter":     h + "  C:/Windows/x.exe\n",
		"stream":           h + "  manager.exe:evil\n",
		"backslash":        h + `  agents\x` + "\n",
		"empty segment":    h + "  agents//x\n",
		"control":          h + "  a\tb\n",
		"crlf":             h + "  x\r\n",
		"no newline":       h + "  x",
		"empty":            "",
		"one space":        h + " x\n",
		"binary mode":      h + " *x\n",
		"short hash":       h[:63] + "  x\n",
		"uppercase hash":   strings.ToUpper(h) + "  x\n",
		"duplicate":        h + "  x\n" + h + "  x\n",
		"duplicate case":   h + "  Manager.exe\n" + h + "  manager.exe\n",
		"lists itself":     h + "  SHA256SUMS\n",
		"lists signature":  h + "  SHA256SUMS.sig\n",
		"lists public key": h + "  release-key.pub\n",
		"blank line":       h + "  x\n\n",
	}
	for name, m := range bad {
		if _, err := ParseManifest([]byte(m)); err == nil {
			t.Errorf("%s: ParseManifest(%q) accepted it", name, m)
		}
	}
	good := h + "  agents/agent-linux-arm64\n" + h + "  manager.exe\n"
	if e, err := ParseManifest([]byte(good)); err != nil || len(e) != 2 || e[0].Path != "agents/agent-linux-arm64" {
		t.Fatalf("ParseManifest(good) = %v, %v", e, err)
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "release-signing.key")
	priv, created, err := LoadOrCreateKey(path)
	if err != nil || !created {
		t.Fatalf("first LoadOrCreateKey = %v, %v", created, err)
	}
	again, created, err := LoadOrCreateKey(path)
	if err != nil || created || !again.Equal(priv) {
		t.Fatalf("second LoadOrCreateKey = %v, %v (same key: %v)", created, err, again.Equal(priv))
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "-----BEGIN PRIVATE KEY-----") {
		t.Fatalf("key file isn't PKCS#8 PEM: %q", data)
	}
	bad := filepath.Join(t.TempDir(), "bad.key")
	writeFile(t, bad, "not a key")
	if _, _, err := LoadOrCreateKey(bad); err == nil {
		t.Fatal("LoadOrCreateKey accepted a garbage file (it must never replace it)")
	}
	if b, _ := os.ReadFile(bad); string(b) != "not a key" {
		t.Fatal("LoadOrCreateKey overwrote an unreadable key file")
	}
}

func TestLoadPublicKey(t *testing.T) {
	priv := newKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	text := EncodePublicKey(pub)
	file := filepath.Join(t.TempDir(), PublicKeyName)
	writeFile(t, file, text+"\r\n")
	for _, v := range []string{text, " " + text + "\n", file} {
		got, err := LoadPublicKey(v)
		if err != nil || !got.Equal(pub) {
			t.Fatalf("LoadPublicKey(%q) = %v, %v", v, got, err)
		}
	}
	// A second spelling of the same key (unused low bits set in the last
	// character) is refused: a key has one text form.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	alias := text[:42] + string(alphabet[strings.IndexByte(alphabet, text[42])+1]) + "="
	if b, err := base64.StdEncoding.DecodeString(alias); err != nil || !ed25519.PublicKey(b).Equal(pub) {
		t.Fatalf("test setup: %q isn't another spelling of the key", alias)
	}
	for _, v := range []string{"", "abc", text[:40], alias, filepath.Join(t.TempDir(), "missing.pub")} {
		if _, err := LoadPublicKey(v); err == nil {
			t.Errorf("LoadPublicKey(%q) accepted it", v)
		}
	}
}
