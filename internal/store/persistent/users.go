package persistent

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"home-harness/internal/domain"
)

// Operator-API users (manager/users.go) and small rotated secrets. Their
// buckets are created on first write, so a database from before them opens
// unchanged and reads as empty; they ride along in Snapshot like the rest,
// so a standby gets the same users.
var usersBucket = []byte("users")
var secretsBucket = []byte("secrets")

// PutUser stores (or replaces) one user, keyed by name.
func (s *Store) PutUser(u domain.User) error {
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("persistent: marshal user: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(usersBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(u.Name), data)
	})
}

// DeleteUser forgets a user.
func (s *Store) DeleteUser(name string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(usersBucket)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(name))
	})
}

// ListUsers returns every stored user.
func (s *Store) ListUsers() ([]domain.User, error) {
	var out []domain.User
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(usersBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, data []byte) error {
			var u domain.User
			if err := json.Unmarshal(data, &u); err != nil {
				return fmt.Errorf("persistent: decode user: %w", err)
			}
			out = append(out, u)
			return nil
		})
	})
	return out, err
}

// GetSecret returns a small secret kept by key (a rotated pairing token).
func (s *Store) GetSecret(key string) (string, bool, error) {
	var val string
	var ok bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(secretsBucket); b != nil {
			if data := b.Get([]byte(key)); data != nil {
				val, ok = string(data), true
			}
		}
		return nil
	})
	return val, ok, err
}

// PutSecret stores a small secret by key.
func (s *Store) PutSecret(key, value string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(secretsBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), []byte(value))
	})
}
