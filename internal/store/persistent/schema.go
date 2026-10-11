package persistent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"

	"home-harness/internal/fsperm"
)

// Schema versions. The layout every manager wrote before versions existed
// is version 1; a database without a version record is therefore version 1
// (and gets one stamped the next time it is opened). A change to what a
// bucket stores, or a new bucket that older code must not run against,
// adds a migration and raises CurrentSchema.
const CurrentSchema = 1

var schemaKey = []byte("schema-version")

// migration brings a database from version To-1 to version To. Up runs in
// one transaction together with the version stamp, so a crash leaves the
// database at the old version or the new one, never between. Because the
// stamp is what decides whether a step runs, a step never runs twice; keep
// each Up idempotent anyway (a restored pre-migration backup re-runs it).
type migration struct {
	To   int
	Name string
	Up   func(tx *bbolt.Tx) error
}

// migrations, in order: migrations[i].To == i+2. Empty while the layout is
// still version 1.
var migrations = []migration{}

// ErrSchemaTooNew is returned (wrapped in a *SchemaError) when the
// database was written by a newer manager than this one.
var ErrSchemaTooNew = errors.New("persistent: database is from a newer manager")

// SchemaError explains a refusal to open a database.
type SchemaError struct {
	Path           string
	Found, Support int
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("the database %s was written by a newer manager (data format %d; this manager understands up to %d). "+
		"Nothing was changed. Install the newer manager again, or restore a backup made before the upgrade", e.Path, e.Found, e.Support)
}

func (e *SchemaError) Unwrap() error { return ErrSchemaTooNew }

// plan is the version and migration list an Open works to; tests pass
// their own.
type plan struct {
	current int
	steps   []migration
}

func defaultPlan() plan { return plan{current: CurrentSchema, steps: migrations} }

func (p plan) validate() error {
	for i, m := range p.steps {
		if m.To != i+2 || m.Up == nil {
			return fmt.Errorf("persistent: migration %d is out of order (To=%d)", i, m.To)
		}
	}
	if p.current != len(p.steps)+1 {
		return fmt.Errorf("persistent: schema %d does not match %d migrations", p.current, len(p.steps))
	}
	return nil
}

func readVersion(tx *bbolt.Tx) (version int, stamped bool) {
	b := tx.Bucket(settingsBucket)
	if b == nil {
		return 0, false
	}
	data := b.Get(schemaKey)
	if len(data) != 8 {
		return 0, false
	}
	return int(binary.BigEndian.Uint64(data)), true
}

func stampVersion(tx *bbolt.Tx, v int) error {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, uint64(v))
	return tx.Bucket(settingsBucket).Put(schemaKey, data)
}

// SchemaVersion is the data-format version recorded in the database.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.View(func(tx *bbolt.Tx) error { v, _ = readVersion(tx); return nil })
	return v, err
}

// prepare checks the database's version before anything else touches it,
// migrating forward when it is older. fresh reports a database with no
// data yet (nothing to protect, nothing to migrate).
func prepare(db *bbolt.DB, path string, p plan) error {
	var found int
	var stamped, fresh bool
	err := db.View(func(tx *bbolt.Tx) error {
		fresh = tx.Bucket(nodesBucket) == nil
		found, stamped = readVersion(tx)
		return nil
	})
	if err != nil {
		return err
	}
	switch {
	case fresh:
		return nil // Open creates the buckets and stamps p.current
	case !stamped:
		found = 1
	case found > p.current:
		return &SchemaError{Path: path, Found: found, Support: p.current}
	}
	if found < 1 {
		return fmt.Errorf("persistent: database %s has an invalid data format version %d", path, found)
	}
	if found == p.current {
		return nil
	}
	// Older: keep a copy of the database as it is, then step forward.
	backup, err := preMigrationBackup(db, path, found, p.current)
	if err != nil {
		return fmt.Errorf("persistent: cannot upgrade %s: the safety copy failed (%w); nothing was changed", path, err)
	}
	for _, m := range p.steps[found-1:] {
		err := db.Update(func(tx *bbolt.Tx) error {
			if err := m.Up(tx); err != nil {
				return err
			}
			return stampVersion(tx, m.To)
		})
		if err != nil {
			return fmt.Errorf("persistent: upgrade step %d (%s) failed: %w; the database is unchanged and a copy from before the upgrade is at %s", m.To, m.Name, err, backup)
		}
	}
	return nil
}

// preMigrationBackup writes a consistent copy next to the database, owner-
// only, and returns its path. It is kept: the operator removes it once the
// upgrade has proved itself.
func preMigrationBackup(db *bbolt.DB, path string, from, to int) (string, error) {
	name := fmt.Sprintf("%s.pre-upgrade-v%d-to-v%d-%s.bak", path, from, to, time.Now().UTC().Format("20060102T150405Z"))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	err = db.View(func(tx *bbolt.Tx) error { _, err := tx.WriteTo(f); return err })
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fsperm.Private(name)
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return filepath.Clean(name), nil
}

// Info is what Inspect learns about a database file.
type Info struct {
	SchemaVersion int  `json:"schemaVersion"`
	Stamped       bool `json:"stamped"`
	Nodes         int  `json:"nodes"`
	Workloads     int  `json:"workloads"`
	Jobs          int  `json:"jobs"`
	Workflows     int  `json:"workflows"`
}

// Inspect opens the database at path read-only, without migrating it,
// checks its structure, and reports its version and size. It is how a
// backup is proven restorable. A database that is open in a running
// manager is reported as such (the exclusive lock times out).
func Inspect(path string) (Info, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return Info{}, fmt.Errorf("persistent: open %s: %w", path, err)
	}
	defer db.Close()
	var info Info
	err = db.View(func(tx *bbolt.Tx) error {
		for e := range tx.Check() {
			return fmt.Errorf("structure check: %w", e)
		}
		info.SchemaVersion, info.Stamped = readVersion(tx)
		if !info.Stamped {
			info.SchemaVersion = 1
		}
		count := func(name []byte) int {
			if b := tx.Bucket(name); b != nil {
				return b.Stats().KeyN
			}
			return 0
		}
		info.Nodes, info.Workloads = count(nodesBucket), count(workloadsBucket)
		info.Jobs, info.Workflows = count(jobsBucket), count(workflowsBucket)
		if tx.Bucket(nodesBucket) == nil {
			return errors.New("not a manager database (no nodes bucket)")
		}
		return nil
	})
	if err != nil {
		return Info{}, fmt.Errorf("persistent: %s: %w", path, err)
	}
	if info.SchemaVersion > CurrentSchema {
		return info, &SchemaError{Path: path, Found: info.SchemaVersion, Support: CurrentSchema}
	}
	return info, nil
}
