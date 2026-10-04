// Package persistent implements the persistent half of v1.md §13: node
// identity, metadata, and last-known manifest, stored so they survive a
// manager restart. It is deliberately separate from the runtime store
// (fast-changing online/offline/heartbeat state), which never touches
// disk — see internal/manager/registry.go.
package persistent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"home-harness/internal/domain"
)

var nodesBucket = []byte("nodes")
var workloadsBucket = []byte("workloads")
var revokedBucket = []byte("revoked")
var nodeMetaBucket = []byte("node-meta")
var jobsBucket = []byte("jobs")
var settingsBucket = []byte("settings")

// auditBuckets maps each audit log to its own bucket, so each is capped
// independently (see AppendAudit).
var auditBuckets = map[domain.AuditLog][]byte{
	domain.AuditSecurity:   []byte("audit"),
	domain.AuditAdmissions: []byte("audit-admissions"),
	domain.AuditNoise:      []byte("audit-noise"),
}

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
		for _, name := range [][]byte{nodesBucket, workloadsBucket, revokedBucket, nodeMetaBucket, jobsBucket, settingsBucket, auditBuckets[domain.AuditSecurity], auditBuckets[domain.AuditAdmissions], auditBuckets[domain.AuditNoise]} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
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

// DeleteWorkloads removes workload records, all in one transaction.
func (s *Store) DeleteWorkloads(ids []domain.WorkloadID) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(workloadsBucket)
		for _, id := range ids {
			if err := b.Delete([]byte(id)); err != nil {
				return fmt.Errorf("persistent: delete workload %s: %w", id, err)
			}
		}
		return nil
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

// RevokeNode records rev in the revocation denylist and forgets the node's
// persisted record, in one transaction: a crash can never leave a node
// both revoked and still seeded as known (or forgotten but not revoked,
// which would let a shared-token launcher silently re-admit it).
func (s *Store) RevokeNode(rev domain.RevokedNode) error {
	data, err := json.Marshal(rev)
	if err != nil {
		return fmt.Errorf("persistent: marshal revocation for %s: %w", rev.NodeID, err)
	}
	err = s.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(revokedBucket).Put([]byte(rev.NodeID), data); err != nil {
			return err
		}
		// Operator metadata belongs to the identity being revoked; a later
		// re-admission starts clean.
		if err := tx.Bucket(nodeMetaBucket).Delete([]byte(rev.NodeID)); err != nil {
			return err
		}
		return tx.Bucket(nodesBucket).Delete([]byte(rev.NodeID))
	})
	if err != nil {
		return fmt.Errorf("persistent: revoke node %s: %w", rev.NodeID, err)
	}
	return nil
}

// ListRevoked returns every persisted revocation, e.g. to rebuild the
// manager's denylist after a restart.
func (s *Store) ListRevoked() ([]domain.RevokedNode, error) {
	var out []domain.RevokedNode
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(revokedBucket).ForEach(func(_, data []byte) error {
			var rev domain.RevokedNode
			if err := json.Unmarshal(data, &rev); err != nil {
				return err
			}
			out = append(out, rev)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("persistent: list revoked: %w", err)
	}
	return out, nil
}

// UnrevokeNode removes id from the revocation denylist. The node's record
// was already forgotten by RevokeNode, so it must be admitted afresh.
func (s *Store) UnrevokeNode(id domain.NodeID) error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(revokedBucket).Delete([]byte(id))
	})
	if err != nil {
		return fmt.Errorf("persistent: unrevoke node %s: %w", id, err)
	}
	return nil
}

// PutNodeMeta stores the operator's metadata for id; empty metadata
// deletes the record.
func (s *Store) PutNodeMeta(id domain.NodeID, meta domain.NodeMeta) error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(nodeMetaBucket)
		if meta.Empty() {
			return b.Delete([]byte(id))
		}
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), data)
	})
	if err != nil {
		return fmt.Errorf("persistent: put node meta %s: %w", id, err)
	}
	return nil
}

// ListNodeMeta returns every node's operator metadata.
func (s *Store) ListNodeMeta() (map[domain.NodeID]domain.NodeMeta, error) {
	out := make(map[domain.NodeID]domain.NodeMeta)
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(nodeMetaBucket).ForEach(func(k, data []byte) error {
			var meta domain.NodeMeta
			if err := json.Unmarshal(data, &meta); err != nil {
				return err
			}
			out[domain.NodeID(k)] = meta
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("persistent: list node meta: %w", err)
	}
	return out, nil
}

// AppendAudit appends e to its log, assigning its sequence number, and
// trims the log to its newest keep entries in the same transaction.
func (s *Store) AppendAudit(e domain.AuditEntry, keep int) (domain.AuditEntry, error) {
	name, ok := auditBuckets[e.Log]
	if !ok {
		return e, fmt.Errorf("persistent: unknown audit log %q", e.Log)
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(name)
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		e.Seq = seq
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err := b.Put(auditKey(seq), data); err != nil {
			return err
		}
		if keep > 0 && seq > uint64(keep) {
			cutoff := seq - uint64(keep)
			c := b.Cursor()
			for k, _ := c.First(); k != nil && binary.BigEndian.Uint64(k) <= cutoff; k, _ = c.Next() {
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return e, fmt.Errorf("persistent: append audit: %w", err)
	}
	return e, nil
}

// ListAudit returns up to limit entries from log, newest first.
func (s *Store) ListAudit(log domain.AuditLog, limit int) ([]domain.AuditEntry, error) {
	name, ok := auditBuckets[log]
	if !ok {
		return nil, fmt.Errorf("persistent: unknown audit log %q", log)
	}
	var out []domain.AuditEntry
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(name).Cursor()
		for k, data := c.Last(); k != nil && (limit <= 0 || len(out) < limit); k, data = c.Prev() {
			var e domain.AuditEntry
			if err := json.Unmarshal(data, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("persistent: list audit: %w", err)
	}
	return out, nil
}

func auditKey(seq uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, seq)
	return k
}

// UpsertJob stores a batch job's request and outcome (its attempts are
// ordinary workload records).
func (s *Store) UpsertJob(job domain.Job) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("persistent: marshal job: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(jobsBucket).Put([]byte(job.ID), data)
	})
}

// ListJobs returns every stored job.
func (s *Store) ListJobs() ([]domain.Job, error) {
	var out []domain.Job
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(jobsBucket).ForEach(func(_, data []byte) error {
			var job domain.Job
			if err := json.Unmarshal(data, &job); err != nil {
				return fmt.Errorf("persistent: decode job: %w", err)
			}
			out = append(out, job)
			return nil
		})
	})
	return out, err
}

var policyKey = []byte("policy")

// GetPolicy returns the stored policy, if one was ever saved.
func (s *Store) GetPolicy() (domain.Policy, bool, error) {
	var p domain.Policy
	found := false
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(settingsBucket).Get(policyKey)
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &p)
	})
	return p, found, err
}

// PutPolicy stores the policy.
func (s *Store) PutPolicy(p domain.Policy) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("persistent: marshal policy: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(settingsBucket).Put(policyKey, data) })
}
