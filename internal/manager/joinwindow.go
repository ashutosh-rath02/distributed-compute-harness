package manager

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// The join window: a new device may ask to join (agent -pair) and the
// join page serves its installers only while it is open. The operator
// opens it from the dashboard ("Add a device") for a few minutes and it
// closes by itself, so on a shared network the page isn't there for
// anyone to swap the downloads of, and strangers can't even queue a
// request. Known devices always reconnect, token and invitation
// admissions are unaffected, and a device already waiting for approval
// keeps its request when the window closes (approving it still works).

const (
	defaultJoinWindow = 15 * time.Minute
	maxJoinWindow     = 2 * time.Hour
)

type joinWindow struct {
	mu    sync.Mutex
	until time.Time
	now   func() time.Time
}

func newJoinWindow() *joinWindow { return &joinWindow{now: time.Now} }

func (w *joinWindow) open(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.until = w.now().Add(d)
}

func (w *joinWindow) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.until = time.Time{}
}

// remaining is how long the window stays open (0 when closed).
func (w *joinWindow) remaining() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if left := w.until.Sub(w.now()); left > 0 {
		return left
	}
	return 0
}

func (s *Server) joinWindowOpen() bool { return s.joinWin.remaining() > 0 }

// OpenJoinWindow lets new devices ask to join for d (capped at two
// hours).
func (s *Server) OpenJoinWindow(d time.Duration) {
	d = min(max(d, time.Minute), maxJoinWindow)
	s.joinWin.open(d)
	log.Printf("manager: adding devices is open for %s", d.Round(time.Second))
	s.publish(domain.EventJoinWindow, "", map[string]any{"open": true, "remainingSeconds": int(d.Seconds())})
}

// CloseJoinWindow stops new devices from asking to join.
func (s *Server) CloseJoinWindow() {
	s.joinWin.close()
	log.Printf("manager: adding devices is closed")
	s.publish(domain.EventJoinWindow, "", map[string]any{"open": false})
}

// openJoinWindowOnFirstRun opens the window when the manager starts with
// no devices at all (a fresh install has to let its first ones in). Run
// calls it after the known devices are loaded, so a restart of a manager
// that has devices starts closed.
func (s *Server) openJoinWindowOnFirstRun() {
	if s.cfg.FirstRunJoinWindow > 0 && len(s.Registry.List()) == 0 {
		s.OpenJoinWindow(s.cfg.FirstRunJoinWindow)
	}
}

type joinWindowView struct {
	Open             bool   `json:"open"`
	RemainingSeconds int    `json:"remainingSeconds"`
	URL              string `json:"url,omitempty"`
	Port             string `json:"port,omitempty"`
}

// apiGetJoinWindow serves GET /join-window.
func (s *Server) apiGetJoinWindow(w http.ResponseWriter, r *http.Request) {
	left := s.joinWin.remaining()
	writeJSON(w, http.StatusOK, joinWindowView{Open: left > 0, RemainingSeconds: int(left.Seconds()), URL: s.joinURL(), Port: s.joinPort()})
}

// apiOpenJoinWindow serves POST /join-window {"minutes": N} (default 15).
func (s *Server) apiOpenJoinWindow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	d := defaultJoinWindow
	if req.Minutes > 0 {
		d = time.Duration(req.Minutes) * time.Minute
	}
	s.OpenJoinWindow(d)
	s.audit(domain.AuditSecurity, "join.window-opened", "", actorFrom(r.Context()), map[string]any{"minutes": int(min(max(d, time.Minute), maxJoinWindow).Minutes())})
	s.apiGetJoinWindow(w, r)
}

// apiCloseJoinWindow serves DELETE /join-window.
func (s *Server) apiCloseJoinWindow(w http.ResponseWriter, r *http.Request) {
	s.CloseJoinWindow()
	s.audit(domain.AuditSecurity, "join.window-closed", "", actorFrom(r.Context()), nil)
	s.apiGetJoinWindow(w, r)
}
