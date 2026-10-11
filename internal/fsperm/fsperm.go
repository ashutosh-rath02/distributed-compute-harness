// Package fsperm makes files readable by their owner only. On Unix that
// is mode 0600, which os.OpenFile already gives; on Windows the mode bits
// mean nothing for access, so the file's access list is replaced with one
// that names only the current user (and stops inheriting the folder's).
// Backups hold the TLS key and the tokens, so they get this treatment.
package fsperm

import "os"

// Private restricts path so only the current user can read or change it.
func Private(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return restrictToOwner(path)
}
