package failover

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// RoleFileName is the role file, kept beside the manager database. It is
// what makes a manager start as a standby (it survives restarts, and is
// written when an active manager steps down) and holds the standby's half
// of the pair: owner-only, since it carries the pair secret and the
// copied pairing token.
const RoleFileName = "standby.json"

// Roles.
const (
	RoleStandby = "standby"
	RoleActive  = "active"
)

// Role is the role file's content.
type Role struct {
	Role string `json:"role"`
	// Primary is the agent-facing address of the manager this standby
	// copies; Fingerprint the TLS identity pinned on that link (the pair's
	// shared one); Secret the pair secret.
	Primary     string `json:"primary,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Secret      string `json:"secret"`
	// PairingToken is the fleet's pairing token, copied from the primary;
	// a promoted manager uses it instead of its own -pairing-token.
	PairingToken string `json:"pairingToken,omitempty"`
	// SeenTerm is the highest term this manager has learned of (copied,
	// fenced by, or promoted to); a promotion goes above it.
	SeenTerm uint64 `json:"seenTerm,omitempty"`
	// SteppedDown: this manager stepped down from term SteppedDownFrom
	// and the active manager hasn't been told yet (HeaderSteppedDown).
	SteppedDown     bool   `json:"steppedDown,omitempty"`
	SteppedDownFrom uint64 `json:"steppedDownFrom,omitempty"`
	// CopiedAt is when a complete copy of the primary's state was last
	// installed here; zero means this manager has no copy to promote (a
	// new standby, or a stepped-down primary whose own state is stale).
	CopiedAt time.Time `json:"copiedAt,omitempty"`
	// Unpaired: the primary refused the secret (another standby was added
	// in this one's place, or it was removed). Such a standby never
	// promotes on its own.
	Unpaired bool `json:"unpaired,omitempty"`
}

// RolePath is the role file for a manager database.
func RolePath(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), RoleFileName) }

// LoadRole reads the role file; nil when there is none.
func LoadRole(dbPath string) (*Role, error) {
	data, err := os.ReadFile(RolePath(dbPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failover: read %s: %w", RoleFileName, err)
	}
	var r Role
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("failover: %s is damaged: %w", RolePath(dbPath), err)
	}
	if r.Role != RoleStandby && r.Role != RoleActive {
		return nil, fmt.Errorf("failover: %s: unknown role %q", RolePath(dbPath), r.Role)
	}
	return &r, nil
}

// SaveRole writes the role file (owner-only, beside its final name, then
// renamed into place).
func SaveRole(dbPath string, r *Role) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := RolePath(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("failover: write %s: %w", RoleFileName, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failover: write %s: %w", RoleFileName, err)
	}
	return nil
}
