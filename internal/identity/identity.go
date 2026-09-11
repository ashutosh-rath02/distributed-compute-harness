// Package identity generates and persists a node's cryptographic identity:
// an Ed25519 keypair whose public key deterministically derives the node's
// persistent NodeID. Loading the same identity directory always yields the
// same NodeID, independent of hostname or IP address (v1.md §4.2).
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"home-harness/internal/domain"
)

const privateKeyFile = "identity.key"

// Identity is a node's private key plus the domain.Identity (NodeID +
// public key) derived from it.
type Identity struct {
	domain.Identity
	PrivateKey ed25519.PrivateKey
}

// Sign signs data with the node's private key.
func (id *Identity) Sign(data []byte) []byte {
	return ed25519.Sign(id.PrivateKey, data)
}

// Verify checks a signature against a domain.Identity's public key. It
// returns false (never panics) for a malformed public key, since callers
// may pass attacker-controlled data (e.g. a manifest received over the
// network) before it has been validated.
func Verify(id domain.Identity, data, sig []byte) bool {
	if len(id.PublicKey) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(id.PublicKey), data, sig)
}

// DeriveNodeID hashes a public key to produce a stable, content-derived
// NodeID: "node-" followed by the first 20 hex characters of its SHA-256.
// Because it is a pure function of the public key, a receiver can
// recompute it independently to check that a claimed NodeID actually
// matches the claimed public key, rather than trusting the pair as given.
func DeriveNodeID(pub ed25519.PublicKey) domain.NodeID {
	sum := sha256.Sum256(pub)
	return domain.NodeID("node-" + hex.EncodeToString(sum[:])[:20])
}

// LoadOrCreate loads the identity persisted under dir, generating and
// persisting a new Ed25519 keypair on first use. dir is created if absent.
func LoadOrCreate(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("identity: create identity dir %q: %w", dir, err)
	}

	keyPath := filepath.Join(dir, privateKeyFile)
	priv, err := loadPrivateKey(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		priv, err = generateAndSave(keyPath)
		if err != nil {
			return nil, err
		}
	}

	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{
		Identity: domain.Identity{
			NodeID:    DeriveNodeID(pub),
			PublicKey: []byte(pub),
		},
		PrivateKey: priv,
	}, nil
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: key file %q has unexpected size %d", path, len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

func generateAndSave(path string) (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate keypair: %w", err)
	}
	// 0o600: private key must not be world/group readable.
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return nil, fmt.Errorf("identity: persist private key to %q: %w", path, err)
	}
	return priv, nil
}
