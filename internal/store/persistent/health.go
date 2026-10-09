package persistent

import "go.etcd.io/bbolt"

// Ping reports whether the database is open and can take a write: it begins
// a write transaction and rolls it back. Nothing is committed, so it costs no
// fsync and does not change Version (a standby would otherwise think the
// database changed on every probe).
func (s *Store) Ping() error {
	tx, err := s.db.Begin(true)
	if err != nil {
		return err
	}
	return tx.Rollback()
}

// SizeBytes is the database file's size.
func (s *Store) SizeBytes() (int64, error) {
	var n int64
	err := s.db.View(func(tx *bbolt.Tx) error {
		n = tx.Size()
		return nil
	})
	return n, err
}
