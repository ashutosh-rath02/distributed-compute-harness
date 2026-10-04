package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

// newAppAgent is an agent that is part of an app (the Android worker):
// no self-update, its app at apk, updates downloaded to update.
func newAppAgent(t *testing.T, apkContent string) (a *Agent, update string) {
	t.Helper()
	dir := t.TempDir()
	apk := filepath.Join(dir, "base.apk")
	if err := os.WriteFile(apk, []byte(apkContent), 0o644); err != nil {
		t.Fatal(err)
	}
	update = filepath.Join(dir, "update", "home-harness.apk")
	a, err := New(nil, Config{IdentityDir: t.TempDir(), Insecure: true, SelfUpdateDisabled: true, AppAPK: apk, AppUpdateFile: update})
	if err != nil {
		t.Fatal(err)
	}
	return a, update
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// An agent updated with its app reports the app it is part of, offers app
// updates, and never offers to replace its own binary.
func TestAppAgentReportsItsAppAndOffersAppUpdates(t *testing.T) {
	a, _ := newAppAgent(t, "app v1")
	if a.appHash != sha("app v1") {
		t.Fatalf("app hash %q", a.appHash)
	}
	f := a.agentFeatures()
	if !slices.Contains(f, domain.FeatureAppUpdate) || slices.Contains(f, domain.FeatureSelfUpdatePath) {
		t.Fatalf("features %v", f)
	}
	// A plain agent offers neither app updates nor an app hash.
	if p := newTestAgent(t); p.appHash != "" || slices.Contains(p.agentFeatures(), domain.FeatureAppUpdate) {
		t.Fatalf("plain agent: %q %v", p.appHash, p.agentFeatures())
	}
}

// The app is downloaded, checked against the hash the manager named, and
// only then appears where the app picks it up; a mismatch leaves nothing.
func TestAppUpdateDownloadsAndChecksTheApp(t *testing.T) {
	served := "app v2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != domain.AppBinaryRoute {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(served))
	}))
	defer srv.Close()
	a, update := newAppAgent(t, "app v1")
	a.setCurrentManagerAddr(strings.TrimPrefix(srv.URL, "http://"))

	res, run := a.selfUpdateCommand(domain.Command{ID: "c1", Name: domain.CommandSelfUpdate, Args: map[string]string{"sha256": sha("app v2"), "path": domain.AppBinaryRoute}})
	if !res.Success || run == nil {
		t.Fatalf("app update refused: %+v", res)
	}
	run()
	if got, err := os.ReadFile(update); err != nil || string(got) != "app v2" {
		t.Fatalf("update file: %q %v", got, err)
	}
	if _, err := os.Stat(update + selfUpdateDownloadSuffix); !os.IsNotExist(err) {
		t.Fatal("the partial download was left behind")
	}

	os.Remove(update)
	served = "tampered"
	_, run = a.selfUpdateCommand(domain.Command{ID: "c2", Name: domain.CommandSelfUpdate, Args: map[string]string{"sha256": sha("app v2"), "path": domain.AppBinaryRoute}})
	run()
	if _, err := os.Stat(update); !os.IsNotExist(err) {
		t.Fatal("an app that failed its hash check was left for the app to install")
	}
	if _, err := os.Stat(update + selfUpdateDownloadSuffix); !os.IsNotExist(err) {
		t.Fatal("a failed download was left behind")
	}
}

// What each kind of agent refuses: an app agent never swaps its binary
// (it sits in the app's read-only install), and a plain agent has no app.
func TestSelfUpdateCommandRefusesWhatTheAgentCantDo(t *testing.T) {
	a, _ := newAppAgent(t, "app v1")
	res, run := a.selfUpdateCommand(domain.Command{ID: "c", Name: domain.CommandSelfUpdate, Args: map[string]string{"sha256": "x", "path": "/agent-binaries/linux/arm64"}})
	if res.Success || run != nil || !strings.Contains(res.Error, "updated with its app") {
		t.Fatalf("binary update on an app agent: %+v", res)
	}
	p := newTestAgent(t)
	res, run = p.selfUpdateCommand(domain.Command{ID: "c", Name: domain.CommandSelfUpdate, Args: map[string]string{"sha256": "x", "path": domain.AppBinaryRoute}})
	if res.Success || run != nil {
		t.Fatalf("app update on a plain agent: %+v", res)
	}
	res, run = p.selfUpdateCommand(domain.Command{ID: "c", Name: domain.CommandSelfUpdate, Args: map[string]string{"sha256": "x", "path": "/agent-binaries/windows/amd64"}})
	if !res.Success || run == nil {
		t.Fatalf("binary update on a plain agent: %+v", res)
	}
}
