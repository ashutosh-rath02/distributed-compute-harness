// Package persistent implements the persistent half of v1.md §13: node
// identity, metadata, and last-known manifest, stored so they survive a
// manager restart. It is deliberately separate from the runtime store
// (fast-changing online/offline/heartbeat state), which never touches
// disk — see internal/manager/registry.go.
package persistent

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"home-harness/internal/domain"
)

var nodesBucket = []byte("nodes")
var workloadsBucket = []byte("workloads")

// Record is what the persistent store keeps for a node: its last-known
// manifest plus registration bookkeeping.
type Record struct {
	Manifest          domain.Manifest `json:"manifest"`
	FirstRegisteredAt time.Time       `json:"firstRegisteredAt"`
	LastRegisteredAt  time.Time       `json:"lastRegisteredAt"`
}

// Store is a BoltDB-backed persistent store, opened from a single file.
type Store struct {
	db *bbolt.DB
}

// Open opens (creating if absent) the persistent store at path.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("persistent: open %q: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(nodesBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(workloadsBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("persistent: init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database file.
func (s *Store) Close() error { return s.db.Close() }

// UpsertNode records a node's manifest as of a successful registration,
// preserving FirstRegisteredAt across repeated calls for the same node.
func (s *Store) UpsertNode(manifest domain.Manifest) error {
	id := manifest.Node.Identity.NodeID
	now := time.Now().UTC()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(nodesBucket)
		rec := Record{Manifest: manifest, FirstRegisteredAt: now, LastRegisteredAt: now}

		if existing := b.Get([]byte(id)); existing != nil {
			var prev Record
			if err := json.Unmarshal(existing, &prev); err == nil {
				rec.FirstRegisteredAt = prev.FirstRegisteredAt
			}
		}

		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("marshal record for %s: %w", id, err)
		}
		return b.Put([]byte(id), data)
	})
}

// GetNode returns the persisted record for a node, if known.
func (s *Store) GetNode(id domain.NodeID) (Record, bool, error) {
	var rec Record
	var found bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(nodesBucket).Get([]byte(id))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("persistent: get node %s: %w", id, err)
	}
	return rec, found, nil
}

// ListNodes returns every persisted node's last-known manifest, e.g. to
// seed the runtime registry with known-but-currently-offline nodes after a
// manager restart. It returns []domain.Manifest (not the fuller Record) so
// that manager.PersistentStore can depend on this method without importing
// this package's Record type.
func (s *Store) ListNodes() ([]domain.Manifest, error) {
	var manifests []domain.Manifest
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(nodesBucket).ForEach(func(_, data []byte) error {
			var rec Record
			if err := json.Unmarshal(data, &rec); err != nil {
				return err
			}
			manifests = append(manifests, rec.Manifest)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("persistent: list nodes: %w", err)
	}
	return manifests, nil
}

// UpsertWorkload records a workload's current request/status, replacing
// any previous record for the same WorkloadID.
func (s *Store) UpsertWorkload(pw domain.PersistedWorkload) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		data, err := json.Marshal(pw)
		if err != nil {
			return fmt.Errorf("marshal workload %s: %w", pw.Workload.ID, err)
		}
		return tx.Bucket(workloadsBucket).Put([]byte(pw.Workload.ID), data)
	})
}

// ListWorkloads returns every persisted workload, e.g. to seed the
// manager's in-memory WorkloadRegistry after a restart.
func (s *Store) ListWorkloads() ([]domain.PersistedWorkload, error) {
	var out []domain.PersistedWorkload
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(workloadsBucket).ForEach(func(_, data []byte) error {
			var pw domain.PersistedWorkload
			if err := json.Unmarshal(data, &pw); err != nil {
				return err
			}
			out = append(out, pw)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("persistent: list workloads: %w", err)
	}
	return out, nil
}

// DeleteNode removes a node's persisted record (e.g. explicit un-trust).
func (s *Store) DeleteNode(id domain.NodeID) error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(nodesBucket).Delete([]byte(id))
	})
	if err != nil {
		return fmt.Errorf("persistent: delete node %s: %w", id, err)
	}
	return nil
}
