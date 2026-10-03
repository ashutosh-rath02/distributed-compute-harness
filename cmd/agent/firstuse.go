package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
)

// pinnedFingerprintFile, in the identity directory, keeps the manager
// certificate a pairing device was admitted by, so later starts pin it
// like -manager-fingerprint.
const pinnedFingerprintFile = "manager-fingerprint"

// firstUse is trust on first use for a pairing agent given no
// -manager-fingerprint (the Android app's worker, which finds the manager
// by LAN discovery). The first certificate it sees is pinned right away,
// for this connection and every later one — including the retries while
// it waits for approval, so nothing else can answer the reconnect after
// the operator taps Approve. The pairing code shown meanwhile is computed
// from that certificate, which is what the operator compares. Once the
// manager admits the device, keep writes the pin to the identity
// directory.
type firstUse struct {
	file string

	mu     sync.Mutex
	pinned string
	kept   bool
}

func (f *firstUse) config() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // verified below: pinned on first use
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("mtls: manager presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.pinned == "" {
				f.pinned = got
				log.Printf("agent: first manager certificate seen: %s (pinned from now on)", got)
				return nil
			}
			if got != f.pinned {
				return fmt.Errorf("mtls: manager certificate fingerprint mismatch: got %s, want %s", got, f.pinned)
			}
			return nil
		},
	}
}

func (f *firstUse) fingerprint() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pinned
}

// keep writes the pin once the manager has admitted this device.
func (f *firstUse) keep() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kept || f.pinned == "" {
		return
	}
	if err := os.WriteFile(f.file, []byte(f.pinned+"\n"), 0o600); err != nil {
		log.Printf("agent: could not keep the manager's certificate fingerprint: %v", err)
		return
	}
	f.kept = true
}
