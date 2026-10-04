package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// PEM encodes a loaded certificate and its private key the way
// LoadOrCreateCert stores them, so a standby manager (roadmap item 17)
// can take over this exact identity.
func PEM(cert tls.Certificate) (certPEM, keyPEM []byte, err error) {
	if len(cert.Certificate) == 0 || cert.PrivateKey == nil {
		return nil, nil, fmt.Errorf("mtls: no certificate loaded")
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: marshal private key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM, nil
}

// Load loads the certificate in dir without ever creating one (a standby
// must not invent an identity of its own).
func Load(dir string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
}

// Install writes certPEM/keyPEM into dir as the manager's identity, after
// checking that they are a matching pair. Each file is written beside its
// final name and renamed into place, the key owner-only.
func Install(dir string, certPEM, keyPEM []byte) (tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: not a certificate and key pair: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: create tls dir %q: %w", dir, err)
	}
	if err := writeReplacing(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeReplacing(filepath.Join(dir, certFile), certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

func writeReplacing(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("mtls: write %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("mtls: replace %q: %w", path, err)
	}
	return nil
}
