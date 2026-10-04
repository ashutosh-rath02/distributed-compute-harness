package failover

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// The standby link: routes on the active manager's agent-facing listener
// (pinned TLS, never the relay), every one authenticated with a bearer
// credential — the one-time enrollment token for RouteEnroll, the pair
// secret for the rest.
const (
	RouteEnroll    = "/standby/enroll"
	RouteSync      = "/standby/sync"
	RouteStatus    = "/standby/status"
	RouteArtifacts = "/standby/artifacts/"

	// HeaderSteppedDown is sent with a sync by a manager that just stepped
	// down (its old term), so the active one records it in its audit log:
	// the stepped-down manager's own log is about to be replaced.
	HeaderSteppedDown = "X-Harness-Stepped-Down"
)

// EnrollRequest is a new standby's first call, with the one-time token.
type EnrollRequest struct {
	Name string `json:"name"`
	// Addr is the standby's agent-facing address (host:port): agents try
	// it when the primary is gone.
	Addr string `json:"addr"`
}

// EnrollResponse hands the standby the pair secret.
type EnrollResponse struct {
	Secret string `json:"secret"`
	Term   uint64 `json:"term"`
}

// Status is what /standby/status answers: an active manager's role and
// term, with its proof.
type Status struct {
	Role      string `json:"role"`
	Term      uint64 `json:"term"`
	TermProof []byte `json:"termProof,omitempty"`
}

// State is the part of a sync bundle that isn't a file.
type State struct {
	Term         uint64 `json:"term"`
	PairingToken string `json:"pairingToken"`
	AIKey        string `json:"aiKey,omitempty"`
	// Artifacts are the SHA-256s of every stored workload file; the
	// standby fetches the ones it lacks and drops the ones no longer there.
	Artifacts []string  `json:"artifacts,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// The bundle's entries. Only these names are ever read, and each is
// written to a path the standby chose, never one from the wire.
const (
	entryState = "state.json"
	entryDB    = "manager.db"
	entryCert  = "tls/manager-cert.pem"
	entryKey   = "tls/manager-key.pem"
)

const (
	maxStateBytes = 8 << 20
	maxPEMBytes   = 64 << 10
	maxDBBytes    = 1 << 30
)

// WriteBundle writes one sync bundle: a zip of the state, the database
// snapshot and the TLS identity.
func WriteBundle(w io.Writer, st State, db io.Reader, certPEM, keyPEM []byte) error {
	zw := zip.NewWriter(w)
	add := func(name string, r io.Reader) error {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	for _, e := range []struct {
		name string
		r    io.Reader
	}{{entryState, bytes.NewReader(data)}, {entryCert, bytes.NewReader(certPEM)}, {entryKey, bytes.NewReader(keyPEM)}, {entryDB, db}} {
		if err := add(e.name, e.r); err != nil {
			return fmt.Errorf("failover: bundle %s: %w", e.name, err)
		}
	}
	return zw.Close()
}

// Bundle is a received sync bundle, read from a local file.
type Bundle struct {
	State   State
	CertPEM []byte
	KeyPEM  []byte
	zr      *zip.ReadCloser
	db      *zip.File
}

// OpenBundle reads a bundle file: the state and the PEMs into memory, the
// database left to ExtractDB. Every required entry must be there once.
func OpenBundle(path string) (*Bundle, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("failover: not a sync bundle: %w", err)
	}
	b := &Bundle{zr: zr}
	seen := map[string]bool{}
	for _, f := range zr.File {
		switch f.Name {
		case entryState, entryCert, entryKey, entryDB:
		default:
			continue // not ours: ignored, never written anywhere
		}
		if seen[f.Name] {
			zr.Close()
			return nil, fmt.Errorf("failover: bundle has %s twice", f.Name)
		}
		seen[f.Name] = true
		switch f.Name {
		case entryDB:
			b.db = f
		case entryState:
			data, err := readEntry(f, maxStateBytes)
			if err == nil {
				err = json.Unmarshal(data, &b.State)
			}
			if err != nil {
				zr.Close()
				return nil, fmt.Errorf("failover: bundle state: %w", err)
			}
		case entryCert, entryKey:
			data, err := readEntry(f, maxPEMBytes)
			if err != nil {
				zr.Close()
				return nil, fmt.Errorf("failover: bundle %s: %w", f.Name, err)
			}
			if f.Name == entryCert {
				b.CertPEM = data
			} else {
				b.KeyPEM = data
			}
		}
	}
	if len(seen) != 4 {
		zr.Close()
		return nil, errors.New("failover: incomplete sync bundle")
	}
	return b, nil
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%s is too large", f.Name)
	}
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is too large", f.Name)
	}
	return data, nil
}

// ExtractDB writes the database snapshot to dest (created owner-only,
// synced to disk before it returns).
func (b *Bundle) ExtractDB(dest string) error {
	if b.db.UncompressedSize64 > maxDBBytes {
		return errors.New("failover: the database in the bundle is too large")
	}
	r, err := b.db.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(r, maxDBBytes))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
	}
	return err
}

// Close releases the bundle file.
func (b *Bundle) Close() error { return b.zr.Close() }
