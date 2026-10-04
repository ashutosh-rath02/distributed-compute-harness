package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
)

// countingTransfer serves files from memory, counting downloads.
type countingTransfer struct {
	files     map[string][]byte
	downloads int
}

func (c *countingTransfer) Fetch(ctx context.Context, ref domain.ArtifactRef, dest string) error {
	c.downloads++
	data, ok := c.files[ref.SHA256]
	if !ok {
		return errors.New("no such file")
	}
	return os.WriteFile(dest, data, 0o600)
}

func (c *countingTransfer) Upload(context.Context, string, string) (domain.ArtifactRef, error) {
	return domain.ArtifactRef{}, errors.New("not used")
}

func refFor(name string, data []byte) domain.ArtifactRef {
	h := sha256.Sum256(data)
	return domain.ArtifactRef{Name: name, SHA256: hex.EncodeToString(h[:]), Size: int64(len(data))}
}

func newTestCache(t *testing.T, max int64) (*inputCache, string) {
	t.Helper()
	root := t.TempDir()
	c := inputCacheIn(root, max)
	c.load()
	return c, root
}

func fetchInto(t *testing.T, xfer ArtifactTransfer, ref domain.ArtifactRef) []byte {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "in")
	if err := xfer.Fetch(t.Context(), ref, dest); err != nil {
		t.Fatalf("Fetch %s: %v", ref.Name, err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf("a .part file was left behind: %v", err)
	}
	return got
}

// The second workload on a file copies it from the cache: no download.
func TestCachedInputIsCopiedNotDownloadedAgain(t *testing.T) {
	c, _ := newTestCache(t, 1<<20)
	data := bytes.Repeat([]byte("frame "), 1000)
	ref := refFor("video.mp4", data)
	net := &countingTransfer{files: map[string][]byte{ref.SHA256: data}}
	xfer := &cachingTransfer{ArtifactTransfer: net, cache: c}

	if got := fetchInto(t, xfer, ref); !bytes.Equal(got, data) || net.downloads != 1 {
		t.Fatalf("first fetch: %d downloads", net.downloads)
	}
	if got := fetchInto(t, xfer, ref); !bytes.Equal(got, data) || net.downloads != 1 {
		t.Fatalf("second fetch downloaded again (%d downloads)", net.downloads)
	}
	if r := c.reportIfChanged(true); len(r.Prefixes) != 1 || r.Prefixes[0] != ref.SHA256[:domain.InputCachePrefixLen] {
		t.Fatalf("report %+v", r)
	}
}

// A cached file that no longer matches its hash (disk damage, a workload
// that wrote to the cache) is never used: it is dropped and downloaded.
func TestTamperedCacheEntryIsDroppedAndDownloadedAgain(t *testing.T) {
	c, _ := newTestCache(t, 1<<20)
	data := []byte("the real contents of the file")
	ref := refFor("doc.txt", data)
	net := &countingTransfer{files: map[string][]byte{ref.SHA256: data}}
	xfer := &cachingTransfer{ArtifactTransfer: net, cache: c}
	fetchInto(t, xfer, ref)

	for _, bad := range [][]byte{
		[]byte("the fake contents of the file"), // same size, other bytes
		[]byte("short"),
	} {
		if err := os.WriteFile(c.path(ref.SHA256), bad, 0o600); err != nil {
			t.Fatal(err)
		}
		before := net.downloads
		if got := fetchInto(t, xfer, ref); !bytes.Equal(got, data) {
			t.Fatalf("workload got tampered bytes %q", got)
		}
		if net.downloads != before+1 {
			t.Fatal("a tampered entry must be replaced by a download")
		}
		// Repaired: the next use is a verified copy again.
		if fetchInto(t, xfer, ref); net.downloads != before+1 {
			t.Fatal("the downloaded file should have been cached again")
		}
	}
}

// The least recently used files go first; a file bigger than the whole
// cache is never kept; one being copied out is never evicted.
func TestCacheEvictsLeastRecentlyUsedWithinItsCap(t *testing.T) {
	c, _ := newTestCache(t, 250)
	mk := func(name string, b byte) (domain.ArtifactRef, []byte) {
		d := bytes.Repeat([]byte{b}, 100)
		return refFor(name, d), d
	}
	a, da := mk("a", 'a')
	b, db := mk("b", 'b')
	d, dd := mk("d", 'd')
	huge := bytes.Repeat([]byte{'h'}, 300)
	h := refFor("huge", huge)
	net := &countingTransfer{files: map[string][]byte{a.SHA256: da, b.SHA256: db, d.SHA256: dd, h.SHA256: huge}}
	xfer := &cachingTransfer{ArtifactTransfer: net, cache: c}

	fetchInto(t, xfer, a)
	time.Sleep(10 * time.Millisecond)
	fetchInto(t, xfer, b)
	time.Sleep(10 * time.Millisecond)
	fetchInto(t, xfer, a) // a is now the most recently used
	time.Sleep(10 * time.Millisecond)
	fetchInto(t, xfer, d) // over the cap: b goes
	held := func(r domain.ArtifactRef) bool { _, ok := c.entries[r.SHA256]; return ok }
	if !held(a) || held(b) || !held(d) || c.used != 200 {
		t.Fatalf("after eviction: a=%v b=%v d=%v used=%d", held(a), held(b), held(d), c.used)
	}
	if _, err := os.Stat(c.path(b.SHA256)); !os.IsNotExist(err) {
		t.Fatal("an evicted file must be deleted")
	}
	fetchInto(t, xfer, h)
	if held(h) || c.used != 200 {
		t.Fatal("a file larger than the cache must not be kept (or evict others)")
	}

	// a and d both busy: nothing can make room for b, so it isn't kept.
	c.entries[a.SHA256].busy, c.entries[d.SHA256].busy = 1, 1
	fetchInto(t, xfer, b)
	if held(b) || !held(a) || !held(d) {
		t.Fatal("files being copied out must not be evicted")
	}
}

// What the manager hears: most recently used first, bounded, and only
// when it changed (but always in full on registering).
func TestCacheReportIsBoundedAndSentOnlyWhenItChanged(t *testing.T) {
	c, _ := newTestCache(t, 1<<30)
	if r := c.reportIfChanged(false); r == nil || len(r.Prefixes) != 0 {
		t.Fatalf("the first report (an empty cache) must still be sent: %+v", r)
	}
	if c.reportIfChanged(false) != nil {
		t.Fatal("nothing changed: no report")
	}
	net := &countingTransfer{files: map[string][]byte{}}
	xfer := &cachingTransfer{ArtifactTransfer: net, cache: c}
	var last domain.ArtifactRef
	for i := 0; i < domain.MaxInputCacheReport+5; i++ {
		data := []byte{byte(i), byte(i >> 8), 'x'}
		last = refFor("f", data)
		net.files[last.SHA256] = data
		fetchInto(t, xfer, last)
	}
	// Recency is by clock; make the last one clearly the newest.
	c.entries[last.SHA256].lastUsed = time.Now().Add(time.Hour)
	r := c.reportIfChanged(false)
	if r == nil || len(r.Prefixes) != domain.MaxInputCacheReport || r.Prefixes[0] != last.SHA256[:domain.InputCachePrefixLen] {
		t.Fatalf("report: %d entries, first %v", len(r.Prefixes), r.Prefixes[:1])
	}
	if c.reportIfChanged(false) != nil {
		t.Fatal("unchanged since the last report")
	}
	if r := c.reportIfChanged(true); r == nil || len(r.Prefixes) != domain.MaxInputCacheReport {
		t.Fatal("registering always sends the full report")
	}
}

// A restarted agent knows what it kept; half-written copies are gone and
// a lowered cap is applied.
func TestCacheSurvivesARestartWithinItsCap(t *testing.T) {
	c, root := newTestCache(t, 1<<20)
	net := &countingTransfer{files: map[string][]byte{}}
	xfer := &cachingTransfer{ArtifactTransfer: net, cache: c}
	var refs []domain.ArtifactRef
	for i, s := range []string{"first file", "second file!"} {
		ref := refFor("f", []byte(s))
		net.files[ref.SHA256] = []byte(s)
		fetchInto(t, xfer, ref)
		refs = append(refs, ref)
		old := time.Now().Add(time.Duration(i-5) * time.Minute)
		os.Chtimes(c.path(ref.SHA256), old, old)
	}
	os.WriteFile(filepath.Join(root, inputCacheDirName, "tmp", "in-123"), []byte("half"), 0o600)

	again := inputCacheIn(root, 1<<20)
	again.load()
	if len(again.entries) != 2 || again.used != int64(len("first file")+len("second file!")) {
		t.Fatalf("after restart: %d entries, %d bytes", len(again.entries), again.used)
	}
	if left, _ := os.ReadDir(filepath.Join(root, inputCacheDirName, "tmp")); len(left) != 0 {
		t.Fatal("half-written copies must be cleared at start")
	}
	xfer = &cachingTransfer{ArtifactTransfer: net, cache: again}
	before := net.downloads
	fetchInto(t, xfer, refs[0])
	if net.downloads != before {
		t.Fatal("a file kept before the restart must be used")
	}

	// The cap lowered to one file: the least recently used goes.
	smaller := inputCacheIn(root, int64(len("second file!")))
	smaller.load()
	if _, ok := smaller.entries[refs[0].SHA256]; !ok || len(smaller.entries) != 1 {
		t.Fatalf("lowered cap: kept %d entries", len(smaller.entries))
	}
}

// The cache lives in the work root under a name cleanWorkRoot skips.
func TestWorkRootCleanupLeavesTheCacheAlone(t *testing.T) {
	c, root := newTestCache(t, 1<<20)
	data := []byte("keep me")
	ref := refFor("k", data)
	net := &countingTransfer{files: map[string][]byte{ref.SHA256: data}}
	fetchInto(t, &cachingTransfer{ArtifactTransfer: net, cache: c}, ref)
	os.MkdirAll(filepath.Join(root, "0123456789abcdef0123456789abcdef"), 0o700)
	cleanWorkRoot(root)
	if _, err := os.Stat(c.path(ref.SHA256)); err != nil {
		t.Fatalf("cleanWorkRoot removed the cache: %v", err)
	}
}
