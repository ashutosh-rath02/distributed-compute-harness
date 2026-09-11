package mtls

import (
	"crypto/tls"
	"testing"
)

func TestLoadOrCreateCertGeneratesOnFirstUse(t *testing.T) {
	cert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected a non-empty certificate chain")
	}
}

func TestLoadOrCreateCertIsStableAcrossReloads(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCert (first): %v", err)
	}
	second, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCert (second): %v", err)
	}

	if Fingerprint(first) != Fingerprint(second) {
		t.Fatalf("expected stable fingerprint across reloads, got %q then %q", Fingerprint(first), Fingerprint(second))
	}
}

func TestDifferentDirsProduceDifferentFingerprints(t *testing.T) {
	a, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert (a): %v", err)
	}
	b, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert (b): %v", err)
	}
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("expected distinct fingerprints for distinct tls dirs")
	}
}

// startTLSEchoServer starts a raw TLS listener using cert and returns its
// address plus a stop function. It's a minimal stand-in for ws.Listen,
// used to test PinnedClientConfig against a real TLS handshake rather
// than calling its callback directly.
func startTLSEchoServer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// tls.Listener.Accept returns before the handshake completes
			// (it happens lazily on first Read/Write) — perform it
			// explicitly so a client's tls.Dial doesn't race a premature
			// close against an unstarted handshake.
			if tlsConn, ok := conn.(*tls.Conn); ok {
				tlsConn.Handshake()
			}
		}
	}()

	return ln.Addr().String()
}

func TestPinnedClientConfigAcceptsMatchingFingerprint(t *testing.T) {
	cert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	addr := startTLSEchoServer(t, cert)

	cfg := PinnedClientConfig(Fingerprint(cert))
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("expected TLS dial to succeed with matching fingerprint, got: %v", err)
	}
	conn.Close()
}

func TestPinnedClientConfigRejectsMismatchedFingerprint(t *testing.T) {
	cert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	addr := startTLSEchoServer(t, cert)

	cfg := PinnedClientConfig("0000000000000000000000000000000000000000000000000000000000000000")
	_, err = tls.Dial("tcp", addr, cfg)
	if err == nil {
		t.Fatal("expected TLS dial to fail with a mismatched fingerprint")
	}
}

func TestFingerprintIsHexSHA256Length(t *testing.T) {
	cert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	fp := Fingerprint(cert)
	if len(fp) != 64 { // SHA-256 = 32 bytes = 64 hex chars
		t.Fatalf("expected a 64-char hex fingerprint, got %d chars: %q", len(fp), fp)
	}
}
