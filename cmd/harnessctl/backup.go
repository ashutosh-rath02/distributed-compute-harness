package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"home-harness/internal/backup"
	"home-harness/internal/fsperm"
)

// backupPassphraseEnv holds the passphrase for backup encryption, so it
// never appears on a command line.
const backupPassphraseEnv = "HARNESS_BACKUP_PASSPHRASE"

func passphraseFrom(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(backupPassphraseEnv), nil
}

// cmdBackup: backup [-to FILE] [-include-artifacts] [-passphrase-file F]
// | backup verify FILE | backup status
func cmdBackup(c *apiClient, args []string) error {
	if len(args) > 0 && args[0] == "status" {
		return c.cmdReliability()
	}
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	to := fs.String("to", "", "where to write the backup (default home-harness-backup-<time>.zip, or .hhbk when encrypted)")
	withArtifacts := fs.Bool("include-artifacts", false, "also back up stored workload files (can be large)")
	passFile := fs.String("passphrase-file", "", "file holding the passphrase to encrypt with (or set "+backupPassphraseEnv+"); without one the backup is NOT encrypted")
	verify := fs.Bool("verify", false, "check an existing backup file (-from) instead of making one")
	from := fs.String("from", "", "with -verify: the backup file")
	if len(args) > 0 && args[0] == "verify" {
		args = append([]string{"-verify"}, args[1:]...)
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			args = append([]string{"-verify", "-from", args[1]}, args[2:]...)
		}
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	pass, err := passphraseFrom(*passFile)
	if err != nil {
		return err
	}
	if *verify {
		return verifyBackupFile(*from, pass)
	}
	if *to == "" {
		ext := ".zip"
		if pass != "" {
			ext = ".hhbk"
		}
		*to = "home-harness-backup-" + time.Now().Format("20060102-150405") + ext
	}
	// The manager's zip arrives in a private temp file; it is verified
	// before it is sealed, so an encrypted backup is never one that
	// could not be opened.
	tmp, err := os.CreateTemp(filepath.Dir(*to), ".backup-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	fsperm.Private(tmp.Name())
	q := ""
	if *withArtifacts {
		q = "?artifacts=1"
	}
	req, _ := http.NewRequest(http.MethodPost, c.base+"/backup"+q, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("backup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		tmp.Close()
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("backup: download: %w", err)
	}
	tmp.Close()
	o, err := backup.OpenFile(tmp.Name(), "")
	if err != nil {
		return err
	}
	rep, err := backup.Verify(o)
	o.Close()
	if err != nil {
		return fmt.Errorf("the manager's backup failed its check: %w", err)
	}
	err = backup.WriteFile(*to, pass, 0, func(w io.Writer) error {
		in, err := os.Open(tmp.Name())
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	})
	if err != nil {
		return err
	}
	st, _ := os.Stat(*to)
	enc := "NOT encrypted: it holds the TLS key and every token, so keep it private"
	if pass != "" {
		enc = "encrypted"
	}
	fmt.Printf("Backup written to %s (%s, %s)\n", *to, humanBytes(uint64(st.Size())), enc)
	fmt.Printf("It holds %d jobs, %d workloads, %d nodes; data format %d; %d files.\n", rep.DB.Jobs, rep.DB.Workloads, rep.DB.Nodes, rep.DB.SchemaVersion, len(rep.Manifest.Entries))
	return nil
}

func verifyBackupFile(path, pass string) error {
	if path == "" {
		return errors.New("usage: harnessctl backup verify <file> [-passphrase-file F]")
	}
	o, err := backup.OpenFile(path, pass)
	if err != nil {
		return err
	}
	defer o.Close()
	rep, err := backup.Verify(o)
	if err != nil {
		return err
	}
	fmt.Printf("OK: made %s, data format %d, %d files; the database opens (%d jobs, %d workloads, %d nodes).\n",
		rep.Manifest.CreatedAt.Local().Format(time.RFC822), rep.DB.SchemaVersion, len(rep.Manifest.Entries), rep.DB.Jobs, rep.DB.Workloads, rep.DB.Nodes)
	return nil
}

func (c *apiClient) cmdReliability() error {
	resp, err := c.http.Get(c.base + "/reliability")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("manager returned %s", resp.Status)
	}
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
	return nil
}
