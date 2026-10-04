package persistent

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"home-harness/internal/domain"
)

// Spot checks and suspect marks (manager/spotcheck.go). Their buckets are
// created on first write, so a database from before them opens unchanged
// and reads as empty.
var spotChecksBucket = []byte("spot-checks")
var suspectsBucket = []byte("suspects")

func spotCheckKey(job domain.JobID, task string) []byte { return []byte(string(job) + "/" + task) }

// UpsertSpotCheck stores one spot check, keyed by its job and task.
func (s *Store) UpsertSpotCheck(c domain.SpotCheck) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("persistent: marshal spot check: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(spotChecksBucket)
		if err != nil {
			return err
		}
		return b.Put(spotCheckKey(c.Job, c.Task), data)
	})
}

// DeleteSpotChecks forgets spot checks (the oldest, past what is kept).
func (s *Store) DeleteSpotChecks(checks []domain.SpotCheck) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(spotChecksBucket)
		if b == nil {
			return nil
		}
		for _, c := range checks {
			if err := b.Delete(spotCheckKey(c.Job, c.Task)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListSpotChecks returns every stored spot check.
func (s *Store) ListSpotChecks() ([]domain.SpotCheck, error) {
	var out []domain.SpotCheck
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(spotChecksBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, data []byte) error {
			var c domain.SpotCheck
			if err := json.Unmarshal(data, &c); err != nil {
				return fmt.Errorf("persistent: decode spot check: %w", err)
			}
			out = append(out, c)
			return nil
		})
	})
	return out, err
}

// PutSuspect stores a node's suspect mark; nil clears it.
func (s *Store) PutSuspect(id domain.NodeID, mark *domain.SuspectMark) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(suspectsBucket)
		if err != nil {
			return err
		}
		if mark == nil {
			return b.Delete([]byte(id))
		}
		data, err := json.Marshal(mark)
		if err != nil {
			return fmt.Errorf("persistent: marshal suspect mark: %w", err)
		}
		return b.Put([]byte(id), data)
	})
}

// ListSuspects returns every node's suspect mark.
func (s *Store) ListSuspects() (map[domain.NodeID]domain.SuspectMark, error) {
	out := map[domain.NodeID]domain.SuspectMark{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(suspectsBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, data []byte) error {
			var m domain.SuspectMark
			if err := json.Unmarshal(data, &m); err != nil {
				return fmt.Errorf("persistent: decode suspect mark: %w", err)
			}
			out[domain.NodeID(k)] = m
			return nil
		})
	})
	return out, err
}
