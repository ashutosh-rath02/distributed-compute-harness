package manager

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/backup"
	"home-harness/internal/domain"
	"home-harness/internal/fsperm"
)

// Reliability: backups, a clean shutdown, and staying useful when the
// disk fills. The pieces that need the on-disk layout (where the TLS key
// and tokens live) are supplied by cmd/manager; everything else is here.

// BackupFunc writes a backup zip of the manager's state to w.
type BackupFunc func(w io.Writer, includeArtifacts bool) error

// storageProber is what a store offers when it watches its own disk
// (internal/store/persistent does); a store without it is assumed fine.
type storageProber interface {
	Probe() error
	Degraded() (bool, string, time.Time)
}

type reliabilityState struct {
	mu       sync.Mutex
	backupFn BackupFunc
	sched    *backup.Schedule
	draining atomic.Bool
	drainAt  time.Time
}

// SetBackup enables POST /backup and says how to make one.
func (s *Server) SetBackup(fn BackupFunc) {
	s.rel.mu.Lock()
	s.rel.backupFn = fn
	s.rel.mu.Unlock()
}

// SetBackupSchedule lets GET /reliability report the scheduled backups.
func (s *Server) SetBackupSchedule(sc *backup.Schedule) {
	s.rel.mu.Lock()
	s.rel.sched = sc
	s.rel.mu.Unlock()
}

// Draining reports whether the manager is shutting down.
func (s *Server) Draining() bool { return s.rel.draining.Load() }

// refuseNewWork answers a submission with a plain reason and returns true
// when the manager must not take new work: it is shutting down, or its
// disk can't record the work (accepting it would promise something a
// restart would lose). Reads and cancellations are never refused.
func (s *Server) refuseNewWork(w http.ResponseWriter) bool {
	if s.Draining() {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "the manager is shutting down and takes no new work; try again in a minute", http.StatusServiceUnavailable)
		return true
	}
	if msg := s.storageProblem(); msg != "" {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "the manager can't save new work right now: "+msg+". Free some space on the manager's disk; it recovers by itself and everything already submitted keeps running.", http.StatusInsufficientStorage)
		return true
	}
	return false
}

// storageProblem is "" when both the database and the file store take
// writes, else a plain description.
func (s *Server) storageProblem() string {
	if p, ok := s.store.(storageProber); ok {
		if bad, reason, _ := p.Degraded(); bad {
			return reason + " (the database can't be written)"
		}
	}
	if st := s.cfg.Artifacts; st != nil {
		if h := st.Health(); h.NoSpace {
			return h.Reason + " (workload files can't be written)"
		}
	}
	return ""
}

// StorageProbeEvery is how often a degraded store is probed for recovery.
var StorageProbeEvery = 5 * time.Second

// watchStorage probes a degraded store every few seconds, so recovery
// needs no restart and no operator action beyond freeing space.
func (s *Server) watchStorage(ctx context.Context) {
	t := time.NewTicker(StorageProbeEvery)
	defer t.Stop()
	was := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		bad := false
		if p, ok := s.store.(storageProber); ok {
			if d, _, _ := p.Degraded(); d {
				bad = p.Probe() != nil
			}
		}
		if st := s.cfg.Artifacts; st != nil && st.Health().NoSpace {
			bad = st.Probe() != nil || bad
		}
		switch {
		case bad && !was:
			log.Printf("manager: STORAGE DEGRADED: %s. New work is refused until space is freed; running work and reads continue", s.storageProblem())
			s.publishStorage(true)
		case !bad && was:
			log.Printf("manager: storage recovered; accepting new work again")
			s.publishStorage(false)
		}
		was = bad
	}
}

func (s *Server) publishStorage(degraded bool) {
	s.audit(domain.AuditSecurity, "storage.degraded", "", "manager", map[string]any{"degraded": degraded})
}

// Drain begins a clean shutdown: new submissions are refused and queued
// work stops being handed out, then it waits up to timeout for work
// already running on devices to finish, and finally closes every device
// connection with a proper close (so agents see an orderly goodbye and
// reconnect with jitter once a manager is back). Work still running when
// the wait ends stays recorded as running and is picked up by the next
// start. The caller then stops the server and closes the store.
func (s *Server) Drain(timeout time.Duration) {
	s.rel.mu.Lock()
	s.rel.drainAt = time.Now()
	s.rel.mu.Unlock()
	s.rel.draining.Store(true)
	deadline := time.Now().Add(timeout)
	for s.Workloads.inFlight() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if s.Workloads.inFlight() {
		log.Printf("manager: drain timed out with work still running; it resumes at the next start")
	}
	s.closeAllConns(3 * time.Second)
}

// closeAllConns closes every device connection in parallel, giving up
// after limit (a peer that isn't reading makes a websocket close wait).
func (s *Server) closeAllConns(limit time.Duration) {
	var wg sync.WaitGroup
	for _, rec := range s.Registry.List() {
		if c := s.Registry.ConnOf(rec.Node.Identity.NodeID); c != nil {
			wg.Add(1)
			go func() { defer wg.Done(); c.Close() }()
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// POST /backup[?artifacts=1]: a consistent backup of the manager's state
// as a zip. It is made into a private temporary file first (so a failure
// is a clean error, and the database's read transaction ends before any
// slow client is served) and then sent. Encrypt it on the client; the
// passphrase never needs to reach the manager.
func (s *Server) apiBackup(w http.ResponseWriter, r *http.Request) {
	s.rel.mu.Lock()
	fn := s.rel.backupFn
	s.rel.mu.Unlock()
	if fn == nil {
		http.Error(w, "this manager can't make backups (no persistent store)", http.StatusServiceUnavailable)
		return
	}
	tmp, err := os.CreateTemp("", "hh-backup-*")
	if err != nil {
		http.Error(w, "backup: "+err.Error(), http.StatusInsufficientStorage)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	fsperm.Private(tmp.Name())
	if err := fn(tmp, r.URL.Query().Get("artifacts") == "1"); err != nil {
		http.Error(w, "backup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	st, err := tmp.Stat()
	if err != nil {
		http.Error(w, "backup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(domain.AuditSecurity, "backup.taken", "", actorFrom(r.Context()), map[string]any{"bytes": st.Size(), "artifacts": r.URL.Query().Get("artifacts") == "1"})
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="home-harness-backup.zip"`)
	w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	if _, err := tmp.Seek(0, io.SeekStart); err == nil {
		io.Copy(w, tmp)
	}
}

type reliabilityView struct {
	Draining bool `json:"draining"`
	Storage  struct {
		Degraded bool   `json:"degraded"`
		Problem  string `json:"problem,omitempty"`
	} `json:"storage"`
	Artifacts *struct {
		Used  int64 `json:"used"`
		Total int64 `json:"total"`
		artifacts.Health
	} `json:"artifacts,omitempty"`
	Backup *backup.Last `json:"scheduledBackup,omitempty"`
}

// GET /reliability: shutdown state, whether the disk takes writes, the
// artifact store's usage and garbage collection, and the last scheduled
// backup.
func (s *Server) apiReliability(w http.ResponseWriter, r *http.Request) {
	var v reliabilityView
	v.Draining = s.Draining()
	if p := s.storageProblem(); p != "" {
		v.Storage.Degraded, v.Storage.Problem = true, p
	}
	if st := s.cfg.Artifacts; st != nil {
		used, total := st.Usage()
		v.Artifacts = &struct {
			Used  int64 `json:"used"`
		Total int64 `json:"total"`
			artifacts.Health
		}{used, total, st.Health()}
	}
	s.rel.mu.Lock()
	sc := s.rel.sched
	s.rel.mu.Unlock()
	if sc != nil {
		l := sc.LastResult()
		v.Backup = &l
	}
	writeJSON(w, http.StatusOK, v)
}
