package persistent

import (
	"fmt"
	"os"
	"time"

	"go.etcd.io/bbolt"

	"home-harness/internal/fsperm"
)

// Compact rewrites the database at path without the free pages bbolt
// keeps after deletes (audit trimming, expired workloads), which never
// shrink the file by themselves. It runs offline, with the database
// closed: swapping the file under a live Store would pull it out from
// under every method at once. The copy is checked before it replaces the
// original; if anything fails (a full disk, say) the original is untouched.
// Returns the file sizes before and after.
func Compact(path string) (before, after int64, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	before = st.Size()
	src, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return 0, 0, fmt.Errorf("persistent: open %s to compact (is a manager running?): %w", path, err)
	}
	tmp := path + ".compacting"
	os.Remove(tmp)
	dst, err := bbolt.Open(tmp, 0o600, nil)
	if err != nil {
		src.Close()
		return 0, 0, err
	}
	err = bbolt.Compact(dst, src, 64<<20)
	dst.Close()
	src.Close()
	if err == nil {
		_, err = Inspect(tmp)
	}
	if err != nil {
		os.Remove(tmp)
		return 0, 0, fmt.Errorf("persistent: compact: %w (the database was not changed)", err)
	}
	fsperm.Private(tmp)
	old := path + ".pre-compact"
	os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		os.Remove(tmp)
		return 0, 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Rename(old, path) // put the original back
		return 0, 0, err
	}
	os.Remove(old)
	st, err = os.Stat(path)
	if err != nil {
		return before, 0, err
	}
	return before, st.Size(), nil
}
