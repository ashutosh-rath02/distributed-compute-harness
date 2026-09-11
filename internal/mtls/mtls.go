// Package mtls provides the manager's TLS identity: a self-signed
// certificate persisted across restarts, plus fingerprint pinning for the
// agent side. This is trust-on-first-use pinning (the same trust model
// the pairing token already uses — a value shared out-of-band), not a CA
// hierarchy: PinnedClientConfig deliberately skips chain validation and
// instead checks the presented leaf certificate's SHA-256 fingerprint
// against one the operator copied from the manager's own startup log.
package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFile = "manager-cert.pem"
	keyFile  = "manager-key.pem"
	// validity is long because this is a self-signed, manually-pinned
	// certificate, not one anything expires/rotates automatically — an
	// expired cert here just means regenerating and re-pinning, same
	// operational cost as changing the pairing token.
	validity = 10 * 365 * 24 * time.Hour
)

// LoadOrCreateCert loads the manager's TLS certificate/key from dir,
// generating and persisting a new self-signed one on first use, so the
// certificate (and therefore its fingerprint) is stable across restarts.
func LoadOrCreateCert(dir string) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: create tls dir %q: %w", dir, err)
	}

	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		return cert, nil
	}
	if !os.IsNotExist(err) {
		return tls.Certificate{}, fmt.Errorf("mtls: load existing cert: %w", err)
	}

	return generateAndSave(certPath, keyPath)
}

// Fingerprint returns the hex-encoded SHA-256 fingerprint of a
// certificate's leaf, for the operator to copy into agents'
// -manager-fingerprint flag.
func Fingerprint(cert tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// PinnedClientConfig returns a *tls.Config for an agent that trusts the
// manager's certificate solely by fingerprint match, not by CA chain.
func PinnedClientConfig(expectedFingerprint string) *tls.Config {
	expected := strings.ToLower(strings.TrimSpace(expectedFingerprint))
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // verified below by pinned fingerprint instead of a CA chain
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("mtls: manager presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != expected {
				return fmt.Errorf("mtls: manager certificate fingerprint mismatch: got %s, want %s", got, expected)
			}
			return nil
		},
	}
}

func generateAndSave(certPath, keyPath string) (tls.Certificate, error) {
	// ECDSA P-256, not Ed25519: confirmed via hardware testing that Windows
	// schannel (curl.exe, PowerShell Invoke-WebRequest — anything that
	// isn't Go's own crypto/tls) cannot complete a handshake against an
	// Ed25519 certificate at all (schannel has never implemented Ed25519
	// support), while Go-to-Go connections were unaffected — the asymmetry
	// that made this easy to miss. P-256 is universally supported and still
	// fully appropriate for TOFU fingerprint pinning (v5's `harnessctl
	// join` script needs a plain Invoke-WebRequest to work against this
	// same cert).
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: generate keypair: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "home-harness-manager"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: create certificate: %w", err)
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: marshal private key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})

	// 0o600: private key must not be world/group readable.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: persist private key to %q: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: persist certificate to %q: %w", certPath, err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: build tls.Certificate: %w", err)
	}
	return cert, nil
}
