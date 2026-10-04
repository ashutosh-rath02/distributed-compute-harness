package persistent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"go.etcd.io/bbolt"

	"home-harness/internal/domain"
)

// Failover state (roadmap item 17). Both live in the settings bucket, so
// a standby's copy of the database carries them.
var termKey = []byte("failover-term")
var standbyPairKey = []byte("standby-pair")

// GetTerm returns the failover term (0 when this state never failed over).
func (s *Store) GetTerm() (uint64, error) {
	var term uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		if data := tx.Bucket(settingsBucket).Get(termKey); len(data) == 8 {
			term = binary.BigEndian.Uint64(data)
		}
		return nil
	})
	return term, err
}

// PutTerm stores the failover term. The commit is fsynced before it
// returns, which is what makes a promotion durable before it serves.
func (s *Store) PutTerm(term uint64) error {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, term)
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(settingsBucket).Put(termKey, data) })
}

// GetStandbyPair returns this state's primary/standby pair, if any.
func (s *Store) GetStandbyPair() (domain.StandbyPair, bool, error) {
	var p domain.StandbyPair
	found := false
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(settingsBucket).Get(standbyPairKey)
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &p)
	})
	return p, found, err
}

// PutStandbyPair stores the pair (replacing any earlier one).
func (s *Store) PutStandbyPair(p domain.StandbyPair) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("persistent: marshal standby pair: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(settingsBucket).Put(standbyPairKey, data) })
}

// DeleteStandbyPair forgets the pair.
func (s *Store) DeleteStandbyPair() error {
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(settingsBucket).Delete(standbyPairKey) })
}

// Snapshot writes a consistent copy of the whole database to w from one
// read transaction, and returns that transaction's ID (the last committed
// write). Write to something fast and local: a long read transaction holds
// up writes that need to grow the file.
func (s *Store) Snapshot(w io.Writer) (uint64, error) {
	var txid uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		txid = uint64(tx.ID())
		_, err := tx.WriteTo(w)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("persistent: snapshot: %w", err)
	}
	return txid, nil
}

// Version is the ID of the last committed write: it changes whenever the
// database does, so a standby can skip copying an unchanged one.
func (s *Store) Version() (uint64, error) {
	var txid uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		txid = uint64(tx.ID())
		return nil
	})
	return txid, err
}

// Path is the database file.
func (s *Store) Path() string { return s.db.Path() }
