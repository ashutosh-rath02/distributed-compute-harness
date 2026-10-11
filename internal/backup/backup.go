// Package backup makes, checks and restores a manager's backup: one zip
// holding the database (a consistent bbolt snapshot), the TLS identity
// agents have pinned, the tokens and, if asked, the stored workload files,
// plus a manifest of every entry's size and SHA-256 and the database's
// data-format version. It can be sealed with a passphrase (crypt.go).
//
// A backup holds the TLS private key and every token: it is written
// owner-only, never served on an unauthenticated route, and should be
// encrypted before it leaves the machine.
package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"home-harness/internal/fsperm"
	"home-harness/internal/store/persistent"
)

// TempDir is where backups stage their scratch files; empty means the
// system temp folder. The manager points it at the database's own folder:
// always writable (the Android app's temp location may not be), and on the
// same volume as the state.
var TempDir string

func cmpTemp(d string) string {
	if d != "" {
		return d
	}
	return TempDir
}

// FormatVersion is the backup file layout. A restore refuses a newer one.
const FormatVersion = 1

const (
	entryDB       = "manager.db"
	entryManifest = "backup-manifest.json"
	entryOperator = "operator-token"
	entryAIKey    = "ai-key"
	entryRemote   = "remote-access"
	entryPairing  = "pairing-token"
	dirTLS        = "tls/"
	dirArtifacts  = "artifacts/"
	maxEntryBytes = 16 << 30
	maxManifest   = 64 << 20
)

// Layout says where each piece of a manager's state lives on disk. A
// backup stores them under fixed names, so one made from any layout
// restores into any other.
type Layout struct {
	DB            string
	TLSDir        string
	OperatorToken string
	AIKey         string
	RemoteAccess  string
	PairingToken  string
	ArtifactDir   string
}

// StateDirLayout is the layout the installers use: everything under one
// directory.
func StateDirLayout(dir string) Layout {
	return Layout{
		DB:            filepath.Join(dir, "manager.db"),
		TLSDir:        filepath.Join(dir, "tls"),
		OperatorToken: filepath.Join(dir, "operator-token"),
		AIKey:         filepath.Join(dir, "ai-key"),
		RemoteAccess:  filepath.Join(dir, "remote-access"),
		PairingToken:  filepath.Join(dir, "pairing-token"),
		ArtifactDir:   filepath.Join(dir, "artifacts"),
	}
}

// Entry is one file in the backup.
type Entry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes a backup; it is the last entry of the zip.
type Manifest struct {
	Format        int       `json:"format"`
	CreatedAt     time.Time `json:"createdAt"`
	SchemaVersion int       `json:"schemaVersion"`
	DBTxID        uint64    `json:"dbTxId"`
	Artifacts     bool      `json:"artifacts"`
	Entries       []Entry   `json:"entries"`
}

// Source is what to back up.
type Source struct {
	Layout Layout
	// SnapshotDB writes a consistent copy of the database and returns its
	// transaction ID. It is called once, to a local temporary file, so the
	// read transaction behind it never waits on the destination.
	SnapshotDB func(io.Writer) (uint64, error)
	// PairingToken is the fleet's pairing token by value when the manager
	// has it only as a flag; it wins over the PairingToken file.
	PairingToken     string
	IncludeArtifacts bool
	// TempDir is where the database snapshot waits; default os.TempDir().
	TempDir string
}

// Create writes the backup zip to w and returns its manifest. The database
// snapshot is proven to open before anything is written to w.
func Create(w io.Writer, src Source) (*Manifest, error) {
	tmp, err := os.CreateTemp(cmpTemp(src.TempDir), "hh-snapshot-*")
	if err != nil {
		return nil, fmt.Errorf("backup: snapshot file: %w", err)
	}
	snapPath := tmp.Name()
	defer os.Remove(snapPath)
	fsperm.Private(snapPath)
	txid, err := src.SnapshotDB(tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("backup: snapshot the database: %w", err)
	}
	info, err := persistent.Inspect(snapPath)
	if err != nil {
		return nil, fmt.Errorf("backup: the snapshot does not check out: %w", err)
	}

	m := &Manifest{Format: FormatVersion, CreatedAt: time.Now().UTC(), SchemaVersion: info.SchemaVersion, DBTxID: txid, Artifacts: src.IncludeArtifacts}
	zw := zip.NewWriter(w)
	add := func(name string, r io.Reader, method uint16, mod time.Time) error {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: mod})
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(fw, h), r)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		m.Entries = append(m.Entries, Entry{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	}
	addFile := func(name, file string, method uint16) error {
		f, err := os.Open(file)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return err
		}
		return add(name, f, method, st.ModTime())
	}

	db, err := os.Open(snapPath)
	if err != nil {
		return nil, err
	}
	err = add(entryDB, db, zip.Deflate, m.CreatedAt)
	db.Close()
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	l := src.Layout
	for _, f := range []struct{ name, file string }{{entryOperator, l.OperatorToken}, {entryAIKey, l.AIKey}, {entryRemote, l.RemoteAccess}} {
		if f.file == "" {
			continue
		}
		if err := addFile(f.name, f.file, zip.Deflate); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
	}
	if src.PairingToken != "" {
		if err := add(entryPairing, strings.NewReader(src.PairingToken), zip.Deflate, m.CreatedAt); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
	} else if l.PairingToken != "" {
		if err := addFile(entryPairing, l.PairingToken, zip.Deflate); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
	}
	if l.TLSDir != "" {
		ents, _ := os.ReadDir(l.TLSDir)
		for _, e := range ents {
			if e.Type().IsRegular() {
				if err := addFile(dirTLS+e.Name(), filepath.Join(l.TLSDir, e.Name()), zip.Deflate); err != nil {
					return nil, fmt.Errorf("backup: %w", err)
				}
			}
		}
	}
	if src.IncludeArtifacts && l.ArtifactDir != "" {
		root := filepath.Join(l.ArtifactDir, "sha256")
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(l.ArtifactDir, p)
			return addFile(dirArtifacts+filepath.ToSlash(rel), p, zip.Store)
		})
		if err != nil {
			return nil, fmt.Errorf("backup: artifacts: %w", err)
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Name < m.Entries[j].Name })
	data, _ := json.MarshalIndent(m, "", "  ")
	fw, err := zw.CreateHeader(&zip.FileHeader{Name: entryManifest, Method: zip.Deflate, Modified: m.CreatedAt})
	if err == nil {
		_, err = fw.Write(data)
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	return m, nil
}

// allowedName accepts only the names Create writes, and nothing that could
// land outside the state: a restore never trusts the file's own paths.
func allowedName(name string) bool {
	switch name {
	case entryDB, entryOperator, entryAIKey, entryRemote, entryPairing:
		return true
	}
	if rest, ok := strings.CutPrefix(name, dirTLS); ok {
		return rest != "" && !strings.ContainsAny(rest, `/\:`) && rest != "." && rest != ".."
	}
	if rest, ok := strings.CutPrefix(name, dirArtifacts); ok {
		clean := path.Clean(rest)
		return rest != "" && clean == rest && !strings.ContainsAny(rest, `\:`) && !strings.HasPrefix(rest, "/") &&
			rest != "." && !strings.HasPrefix(rest, "../") && strings.HasPrefix(rest, "sha256/")
	}
	return false
}

// Report is what Verify learned.
type Report struct {
	Manifest Manifest
	DB       persistent.Info
}

// Open is a backup file ready to read as a zip: decrypted into a private
// temporary file if it was sealed.
type Open struct {
	F         *os.File
	Size      int64
	Encrypted bool
	cleanup   string
}

// OpenFile opens path, decrypting with passphrase if it is encrypted.
func OpenFile(p, passphrase string) (*Open, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	head := make([]byte, len(encMagic))
	n, _ := io.ReadFull(f, head)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if !IsEncrypted(head[:n]) {
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		return &Open{F: f, Size: st.Size()}, nil
	}
	defer f.Close()
	dr, err := NewDecryptReader(f, passphrase)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(TempDir, "hh-restore-*")
	if err != nil {
		return nil, err
	}
	fsperm.Private(tmp.Name())
	size, err := io.Copy(tmp, dr)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	return &Open{F: tmp, Size: size, Encrypted: true, cleanup: tmp.Name()}, nil
}

// Close releases the file (and removes a decrypted copy).
func (o *Open) Close() {
	o.F.Close()
	if o.cleanup != "" {
		os.Remove(o.cleanup)
	}
}

func readManifest(zr *zip.Reader) (*Manifest, error) {
	for _, f := range zr.File {
		if f.Name != entryManifest {
			continue
		}
		if f.UncompressedSize64 > maxManifest {
			return nil, errors.New("backup: manifest too large")
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		var m Manifest
		if err := json.NewDecoder(io.LimitReader(r, maxManifest)).Decode(&m); err != nil {
			return nil, fmt.Errorf("backup: manifest: %w", err)
		}
		return &m, nil
	}
	return nil, errors.New("backup: no manifest: not a backup made by 'manager backup' (a -export-state zip is restored with -import-state)")
}

// Verify checks a backup without touching any state: the format version,
// every entry present with the size and SHA-256 the manifest records, no
// stray entries, and the database inside opens and passes bbolt's
// structure check (a dry-run restore of the one part that matters most).
func Verify(o *Open) (*Report, error) {
	zr, err := zip.NewReader(o.F, o.Size)
	if err != nil {
		return nil, fmt.Errorf("backup: not a backup file: %w", err)
	}
	m, err := readManifest(zr)
	if err != nil {
		return nil, err
	}
	if m.Format > FormatVersion || m.Format < 1 {
		return nil, fmt.Errorf("backup: this backup has file format %d; this manager reads up to %d", m.Format, FormatVersion)
	}
	want := map[string]Entry{}
	for _, e := range m.Entries {
		if !allowedName(e.Name) {
			return nil, fmt.Errorf("backup: refusing entry %q", e.Name)
		}
		want[e.Name] = e
	}
	seen := map[string]bool{}
	var dbTmp string
	defer func() {
		if dbTmp != "" {
			os.Remove(dbTmp)
		}
	}()
	rep := &Report{Manifest: *m}
	for _, f := range zr.File {
		if f.Name == entryManifest {
			continue
		}
		e, ok := want[f.Name]
		if !ok || seen[f.Name] {
			return nil, fmt.Errorf("backup: entry %q is not in the manifest (or appears twice)", f.Name)
		}
		seen[f.Name] = true
		if int64(f.UncompressedSize64) != e.Size || e.Size > maxEntryBytes {
			return nil, fmt.Errorf("backup: %s has the wrong size", f.Name)
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		var dst io.Writer = h
		var dbFile *os.File
		if f.Name == entryDB {
			if dbFile, err = os.CreateTemp(TempDir, "hh-verify-*"); err != nil {
				r.Close()
				return nil, err
			}
			dbTmp = dbFile.Name()
			fsperm.Private(dbTmp)
			dst = io.MultiWriter(h, dbFile)
		}
		_, err = io.Copy(dst, io.LimitReader(r, e.Size+1))
		r.Close()
		if dbFile != nil {
			dbFile.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("backup: %s: %w", f.Name, err)
		}
		if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return nil, fmt.Errorf("backup: %s does not match its checksum: the backup is damaged", f.Name)
		}
	}
	for name := range want {
		if !seen[name] {
			return nil, fmt.Errorf("backup: %s is listed but missing: the backup is incomplete", name)
		}
	}
	if dbTmp == "" {
		return nil, errors.New("backup: no database in the backup")
	}
	rep.DB, err = persistent.Inspect(dbTmp)
	if err != nil {
		return nil, fmt.Errorf("backup: the database in the backup: %w", err)
	}
	return rep, nil
}
