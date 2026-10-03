package manager

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func TestJoinWindowOpensAndClosesByItself(t *testing.T) {
	w := newJoinWindow()
	clock := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }
	if w.remaining() != 0 {
		t.Fatal("a new window must start closed")
	}
	w.open(15 * time.Minute)
	clock = clock.Add(14 * time.Minute)
	if got := w.remaining(); got != time.Minute {
		t.Fatalf("remaining %s, want 1m", got)
	}
	clock = clock.Add(time.Minute)
	if w.remaining() != 0 {
		t.Fatal("the window must close by itself when its time is up")
	}
	w.open(time.Hour)
	w.close()
	if w.remaining() != 0 {
		t.Fatal("close must close it at once")
	}
}

// A fresh manager has to let its first devices in; a manager that already
// knows devices starts closed (a restart must not reopen it).
func TestJoinWindowOpensOnlyOnFirstRun(t *testing.T) {
	fresh := NewServer(nil, nil, Config{FirstRunJoinWindow: 15 * time.Minute})
	fresh.openJoinWindowOnFirstRun()
	if !fresh.joinWindowOpen() {
		t.Fatal("a manager with no devices must open adding devices")
	}
	known := NewServer(nil, nil, Config{FirstRunJoinWindow: 15 * time.Minute})
	known.Registry.Seed(domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Node: domain.Node{Identity: domain.Identity{NodeID: "node-known"}, Name: "laptop"}})
	known.openJoinWindowOnFirstRun()
	if known.joinWindowOpen() {
		t.Fatal("a manager that knows devices must start closed")
	}
	off := NewServer(nil, nil, Config{})
	off.openJoinWindowOnFirstRun()
	if off.joinWindowOpen() {
		t.Fatal("no first-run window configured: closed")
	}
}

// Closed: the join page shows how to open it and serves nothing else.
func TestJoinPageServesNothingWhileClosed(t *testing.T) {
	s := newJoinPageServer(t, "")
	s.CloseJoinWindow()
	h := s.JoinPageHandler()
	page := joinGet(t, h, "192.168.1.20:7419", "/", "Windows")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Adding devices is switched off") || strings.Contains(page.Body.String(), "irm ") {
		t.Fatalf("closed page: %d %s", page.Code, page.Body.String())
	}
	for _, path := range []string{"/install/windows.ps1", "/install/macos.sh", "/agent/windows/amd64", "/app.apk"} {
		if rec := joinGet(t, h, "192.168.1.20:7419", path, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s while closed: %d, want 403", path, rec.Code)
		}
	}
	s.OpenJoinWindow(time.Minute)
	if rec := joinGet(t, h, "192.168.1.20:7419", "/install/windows.ps1", ""); rec.Code != http.StatusOK {
		t.Fatalf("open again: %d", rec.Code)
	}
}

// The window only gates new requests: a device already waiting keeps its
// place, and approving it after the window closed still lets it in.
func TestJoinWindowOnlyGatesNewRequests(t *testing.T) {
	j, _ := newTestJoinRequests()
	if d, _ := j.request(req("node-late", "10.0.0.2"), false); d != joinClosed {
		t.Fatalf("a new device while closed: %v, want closed", d)
	}
	if len(j.list()) != 0 {
		t.Fatal("a refused device must not become a request")
	}
	j.request(req("node-early", "10.0.0.3"), true) // asked while open
	if d, _ := j.request(req("node-early", "10.0.0.3"), false); d != joinPending {
		t.Fatalf("a waiting device after the window closed: %v, want still pending", d)
	}
	j.approve("node-early")
	if d, _ := j.request(req("node-early", "10.0.0.3"), false); d != joinApproved {
		t.Fatalf("approved after the window closed: %v, want admitted", d)
	}
}
