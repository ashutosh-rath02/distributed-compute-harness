package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"home-harness/internal/backup"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
)

// Subcommands that work on a manager's state while it is stopped:
//
//	manager backup  -to FILE [-include-artifacts] [-passphrase-file F]
//	manager restore -from FILE [-force] [-dry-run] [-passphrase-file F]
//	manager compact -db FILE
//
// They take either -state-dir (the installers' layout: manager.db, tls/,
// operator-token, ... in one folder) or the same per-file flags the
// manager itself takes, with the same defaults.

// layoutFlags registers the flags that say where the state lives.
func layoutFlags(fs *flag.FlagSet) func() backup.Layout {
	stateDir := fs.String("state-dir", "", "the state directory (manager.db, tls/, operator-token, ai-key, pairing-token, artifacts/); alternatively give the individual paths below")
	db := fs.String("db", "harness-manager.db", "path to the persistent store file")
	tlsDir := fs.String("tls-dir", "harness-manager-tls", "directory holding the manager's TLS certificate")
	opTok := fs.String("operator-token-file", "harness-operator-token", "the operator token file")
	aiKey := fs.String("ai-key-file", "", "the AI key file (default \"ai-key\" next to the operator token)")
	remote := fs.String("remote-access-file", "", "the remote access file (default \"remote-access\" next to the operator token)")
	pairing := fs.String("pairing-token-file", "", "file holding the pairing token (default \"pairing-token\" next to the operator token)")
	art := fs.String("artifact-dir", "harness-artifacts", "the workload file store")
	return func() backup.Layout {
		if *stateDir != "" {
			return backup.StateDirLayout(*stateDir)
		}
		next := func(v, name string) string {
			if v != "" {
				return v
			}
			return filepath.Join(filepath.Dir(*opTok), name)
		}
		return backup.Layout{DB: *db, TLSDir: *tlsDir, OperatorToken: *opTok, AIKey: next(*aiKey, "ai-key"),
			RemoteAccess: next(*remote, "remote-access"), PairingToken: next(*pairing, "pairing-token"), ArtifactDir: *art}
	}
}

func readPassphrase(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv("HARNESS_BACKUP_PASSPHRASE"), nil
}

// offlineSnapshot copies the database from a read-only open. It fails
// (rather than waits) while a manager has the database: use
// "harnessctl backup" for a running one.
func offlineSnapshot(path string) func(io.Writer) (uint64, error) {
	return func(w io.Writer) (uint64, error) {
		db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
		if errors.Is(err, bbolt.ErrTimeout) {
			return 0, errors.New("a manager is running on this database: stop it, or take the backup through the running manager with \"harnessctl backup\"")
		}
		if err != nil {
			return 0, err
		}
		defer db.Close()
		var id uint64
		err = db.View(func(tx *bbolt.Tx) error {
			id = uint64(tx.ID())
			_, err := tx.WriteTo(w)
			return err
		})
		return id, err
	}
}

// runSubcommand handles the offline subcommands; it reports whether args
// named one.
func runSubcommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	var err error
	switch args[0] {
	case "backup":
		err = cmdBackupOffline(args[1:])
	case "restore":
		err = cmdRestore(args[1:])
	case "compact":
		err = cmdCompact(args[1:])
	default:
		return false
	}
	if err != nil {
		log.Fatalf("manager %s: %v", args[0], err)
	}
	return true
}

func cmdBackupOffline(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	layout := layoutFlags(fs)
	to := fs.String("to", "", "the backup file to write (required)")
	arts := fs.Bool("include-artifacts", false, "also back up stored workload files (can be large)")
	passFile := fs.String("passphrase-file", "", "file holding a passphrase to encrypt with (or set HARNESS_BACKUP_PASSPHRASE); without one the backup is NOT encrypted")
	fs.Parse(args)
	if *to == "" {
		return errors.New("-to FILE is required")
	}
	pass, err := readPassphrase(*passFile)
	if err != nil {
		return err
	}
	l := layout()
	var m *backup.Manifest
	err = backup.WriteFile(*to, pass, 0, func(w io.Writer) error {
		var err error
		m, err = backup.Create(w, backup.Source{Layout: l, SnapshotDB: offlineSnapshot(l.DB), IncludeArtifacts: *arts})
		return err
	})
	if err != nil {
		return err
	}
	// Prove it: read the file back the way a restore would.
	o, err := backup.OpenFile(*to, pass)
	if err == nil {
		_, err = backup.Verify(o)
		o.Close()
	}
	if err != nil {
		os.Remove(*to)
		return fmt.Errorf("the backup failed its own check and was removed: %w", err)
	}
	enc := "NOT encrypted (it holds the TLS key and every token: keep it private)"
	if pass != "" {
		enc = "encrypted"
	}
	fmt.Printf("Backup written to %s: %d files, data format %d, %s. Verified.\n", *to, len(m.Entries), m.SchemaVersion, enc)
	return nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	layout := layoutFlags(fs)
	from := fs.String("from", "", "the backup file to restore (required)")
	force := fs.Bool("force", false, "replace an existing database (the old files are kept as .pre-restore)")
	dry := fs.Bool("dry-run", false, "only check the backup: checksums, version, and that its database opens")
	passFile := fs.String("passphrase-file", "", "file holding the passphrase (or set HARNESS_BACKUP_PASSPHRASE)")
	fs.Parse(args)
	if *from == "" {
		return errors.New("-from FILE is required")
	}
	pass, err := readPassphrase(*passFile)
	if err != nil {
		return err
	}
	o, err := backup.OpenFile(*from, pass)
	if err != nil {
		return err
	}
	defer o.Close()
	if *dry {
		rep, err := backup.Verify(o)
		if err != nil {
			return err
		}
		fmt.Printf("OK (dry run, nothing changed): made %s, data format %d, %d files; the database opens with %d jobs, %d workloads, %d nodes.\n",
			rep.Manifest.CreatedAt.Local().Format(time.RFC822), rep.DB.SchemaVersion, len(rep.Manifest.Entries), rep.DB.Jobs, rep.DB.Workloads, rep.DB.Nodes)
		return nil
	}
	rep, skipped, err := backup.Restore(o, layout(), *force)
	if err != nil {
		return err
	}
	fmt.Printf("Restored %d files (made %s, data format %d). Start the manager again; agents reconnect on their own.\n",
		len(rep.Manifest.Entries)-len(skipped), rep.Manifest.CreatedAt.Local().Format(time.RFC822), rep.DB.SchemaVersion)
	if len(skipped) > 0 {
		fmt.Printf("Not restored (this layout has nowhere for them): %s\n", strings.Join(skipped, ", "))
	}
	return nil
}

func cmdCompact(args []string) error {
	fs := flag.NewFlagSet("compact", flag.ExitOnError)
	db := fs.String("db", "harness-manager.db", "the database file (the manager must be stopped)")
	fs.Parse(args)
	before, after, err := persistent.Compact(*db)
	if err != nil {
		return err
	}
	fmt.Printf("Compacted %s: %d -> %d bytes.\n", *db, before, after)
	return nil
}

// compactIfLarge runs at startup, before the database is opened: a file
// over limit bytes is compacted (the manager is not serving yet, so
// nothing can be using it). Failure only logs; the old file stays.
func compactIfLarge(path string, limit int64) {
	st, err := os.Stat(path)
	if err != nil || limit <= 0 || st.Size() < limit {
		return
	}
	before, after, err := persistent.Compact(path)
	if err != nil {
		log.Printf("manager: startup compaction skipped: %v", err)
		return
	}
	log.Printf("manager: compacted the database at start: %d -> %d bytes", before, after)
}

// wireBackup gives the running manager its online backup and, if asked,
// the schedule.
func wireBackup(srv *manager.Server, store *persistent.Store, l backup.Layout, pairingToken string, dir string, every time.Duration, keep int, passFile string, arts bool) *backup.Schedule {
	mk := func(w io.Writer, withArtifacts bool) error {
		_, err := backup.Create(w, backup.Source{Layout: l, SnapshotDB: store.Snapshot, PairingToken: pairingToken, IncludeArtifacts: withArtifacts})
		return err
	}
	srv.SetBackup(mk)
	if dir == "" {
		return nil
	}
	pass, err := readPassphrase(passFile)
	if err != nil {
		log.Fatalf("manager: -backup-passphrase-file: %v", err)
	}
	if every <= 0 {
		every = 24 * time.Hour
	}
	if keep <= 0 {
		keep = 7
	}
	if pass == "" {
		log.Printf("manager: WARNING scheduled backups in %s are NOT encrypted (they hold the TLS key and tokens); set -backup-passphrase-file", dir)
	}
	sc := &backup.Schedule{Dir: dir, Every: every, Keep: keep, Passphrase: pass,
		Make: func(w io.Writer) error { return mk(w, arts) }}
	srv.SetBackupSchedule(sc)
	log.Printf("manager: backing up to %s every %s, keeping %d", dir, every, keep)
	return sc
}
