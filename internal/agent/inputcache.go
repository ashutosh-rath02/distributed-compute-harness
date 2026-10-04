package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// The input cache (domain.FeatureInputCache): input files this device
// downloaded are kept, up to a size cap, so more work on the same file —
// a second job over the same video, a re-run — copies it from this
// device's disk instead of downloading it again. The manager is told
// which files are here and prefers this device for such work when it is
// no busier than the others (manager/locality.go).
//
// A cached file is trusted no more than the network: it is copied into
// the workload's directory through the same size and SHA-256 check as a
// download, and one that doesn't match is dropped and downloaded again.
// Copies, never hard links: a workload may write to its inputs, and a
// link would let it change the cached file (and another workload's input)
// after it was checked. Least recently used files go first when it is
// full; a file larger than the whole cache is never kept.

// inputCacheDirName is the cache's folder inside the work root: not
// shaped like a workload ID, so cleanWorkRoot leaves it alone.
const inputCacheDirName = "input-cache"

type inputCache struct {
	dir string
	max int64

	mu      sync.Mutex
	entries map[string]*cachedInput // by SHA-256
	used    int64
	// gen counts changes to what is held; reported is the gen the manager
	// was last told about (reportIfChanged).
	gen, reported uint64
	loaded        bool
}

type cachedInput struct {
	size     int64
	lastUsed time.Time
	// busy counts copies out in progress: such a file is never evicted
	// (and on Windows couldn't be removed anyway).
	busy int
}

func newInputCache(dir string, max int64) *inputCache {
	return &inputCache{dir: dir, max: max, entries: map[string]*cachedInput{}}
}

// inputCacheIn is the cache kept in workRoot.
func inputCacheIn(workRoot string, max int64) *inputCache {
	return newInputCache(filepath.Join(workRoot, inputCacheDirName), max)
}

func (c *inputCache) path(sha string) string {
	return filepath.Join(c.dir, "sha256", sha[:2], sha)
}

// load indexes what an earlier run of this agent left, drops half-written
// copies, and trims to the cap (it may have been lowered). Run calls it,
// not New: a standby copy must not touch the running agent's files.
func (c *inputCache) load() {
	tmp := filepath.Join(c.dir, "tmp")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		log.Printf("agent: input cache disabled: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	filepath.WalkDir(filepath.Join(c.dir, "sha256"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		sha := d.Name()
		if !domain.ValidSHA256(sha) || filepath.Base(filepath.Dir(path)) != sha[:2] {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		c.entries[sha] = &cachedInput{size: fi.Size(), lastUsed: fi.ModTime()}
		c.used += fi.Size()
		return nil
	})
	c.makeRoomLocked(0)
	c.loaded = true
	c.gen++
}

// copyOut places a cached ref at dest, checked against ref's size and
// SHA-256 on the way. False when it isn't cached, or the cached copy
// didn't match (then it is dropped): the caller downloads it instead.
func (c *inputCache) copyOut(ctx context.Context, ref domain.ArtifactRef, dest string) bool {
	if !domain.ValidSHA256(ref.SHA256) {
		return false
	}
	c.mu.Lock()
	e, ok := c.entries[ref.SHA256]
	if !ok || !c.loaded {
		c.mu.Unlock()
		return false
	}
	e.busy++
	c.mu.Unlock()

	err := copyChecked(ctx, c.path(ref.SHA256), dest, ref)

	c.mu.Lock()
	defer c.mu.Unlock()
	e.busy--
	switch {
	case err == nil:
		now := time.Now()
		e.lastUsed = now
		os.Chtimes(c.path(ref.SHA256), now, now) // recency survives a restart
		if len(c.entries) > domain.MaxInputCacheReport {
			c.gen++ // which files make the (bounded) report may have changed
		}
		return true
	case ctx.Err() != nil:
		return false // the workload ended; the cached file may be fine
	default:
		log.Printf("agent: input cache: dropping %s: %v", ref.SHA256[:12], err)
		if e.busy == 0 && c.entries[ref.SHA256] == e {
			os.Remove(c.path(ref.SHA256))
			delete(c.entries, ref.SHA256)
			c.used -= e.size
			c.gen++
		}
		return false
	}
}

// keep copies a freshly downloaded and verified input at src into the
// cache (checking it again on the way), making room by dropping the least
// recently used files. Best effort: a file that doesn't fit is not kept.
func (c *inputCache) keep(ref domain.ArtifactRef, src string) {
	if !domain.ValidSHA256(ref.SHA256) || ref.Size <= 0 || ref.Size > c.max {
		return
	}
	c.mu.Lock()
	_, have := c.entries[ref.SHA256]
	loaded := c.loaded
	c.mu.Unlock()
	if have || !loaded {
		return
	}
	tmp, err := os.CreateTemp(filepath.Join(c.dir, "tmp"), "in-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName) // gone already once it is renamed into place
	if err := copyChecked(context.Background(), src, tmpName, ref); err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, have := c.entries[ref.SHA256]; have || !c.makeRoomLocked(ref.Size) {
		return
	}
	final := c.path(ref.SHA256)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return
	}
	if err := os.Rename(tmpName, final); err != nil {
		return
	}
	c.entries[ref.SHA256] = &cachedInput{size: ref.Size, lastUsed: time.Now()}
	c.used += ref.Size
	c.gen++
}

// makeRoomLocked drops least recently used files until n more bytes fit
// under the cap, skipping any being copied out (or that can't be removed
// right now). False if it couldn't.
func (c *inputCache) makeRoomLocked(n int64) bool {
	if c.used+n <= c.max {
		return true
	}
	byAge := make([]string, 0, len(c.entries))
	for sha, e := range c.entries {
		if e.busy == 0 {
			byAge = append(byAge, sha)
		}
	}
	sort.Slice(byAge, func(i, j int) bool { return c.entries[byAge[i]].lastUsed.Before(c.entries[byAge[j]].lastUsed) })
	for _, sha := range byAge {
		if c.used+n <= c.max {
			break
		}
		if err := os.Remove(c.path(sha)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		c.used -= c.entries[sha].size
		delete(c.entries, sha)
		c.gen++
	}
	return c.used+n <= c.max
}

// reportIfChanged is what to tell the manager about the cache: the most
// recently used files' SHA-256 prefixes, bounded. With always (on
// registering) it is always the full report; otherwise nil unless what is
// held changed since the last report.
func (c *inputCache) reportIfChanged(always bool) *domain.InputCacheReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !always && c.gen == c.reported {
		return nil
	}
	c.reported = c.gen
	shas := make([]string, 0, len(c.entries))
	for sha := range c.entries {
		shas = append(shas, sha)
	}
	sort.Slice(shas, func(i, j int) bool { return c.entries[shas[i]].lastUsed.After(c.entries[shas[j]].lastUsed) })
	if len(shas) > domain.MaxInputCacheReport {
		shas = shas[:domain.MaxInputCacheReport]
	}
	report := &domain.InputCacheReport{Prefixes: make([]string, len(shas))}
	for i, sha := range shas {
		report.Prefixes[i] = sha[:domain.InputCachePrefixLen]
	}
	return report
}

// copyChecked copies src to dest through ref's size and SHA-256 check,
// via dest.part, so dest only ever appears with the right bytes.
func copyChecked(ctx context.Context, src, dest string, ref domain.ArtifactRef) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	part := dest + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(ctxReader{ctx, in}, ref.Size+1))
	closeErr := out.Close()
	switch {
	case copyErr != nil:
		err = copyErr
	case closeErr != nil:
		err = closeErr
	case n != ref.Size:
		err = fmt.Errorf("%d bytes, expected %d", n, ref.Size)
	case hex.EncodeToString(h.Sum(nil)) != ref.SHA256:
		err = errors.New("content does not match its sha256")
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dest)
}

// ctxReader stops a long local copy when its workload is canceled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// cachingTransfer is an assignment's transfer that takes inputs from the
// cache when it has them and keeps what it downloads.
type cachingTransfer struct {
	ArtifactTransfer
	cache    *inputCache
	node     domain.NodeID
	workload domain.WorkloadID
}

func (t *cachingTransfer) Fetch(ctx context.Context, ref domain.ArtifactRef, dest string) error {
	if t.cache.copyOut(ctx, ref, dest) {
		log.Printf("agent %s: workload %s: input %s copied from this device's cache (%d bytes, not downloaded)", t.node, t.workload, ref.Name, ref.Size)
		return nil
	}
	if err := t.ArtifactTransfer.Fetch(ctx, ref, dest); err != nil {
		return err
	}
	t.cache.keep(ref, dest)
	return nil
}
