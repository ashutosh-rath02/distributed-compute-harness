package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

// The Android worker, end to end with a real manager and agent: the app's
// agent reports the app it is part of; the manager, offering a newer app,
// shows it as outdated; an update makes the agent download the app,
// checked, to where the app installs it; once the newer app runs, it is
// current.
func TestAppWorkerDownloadsTheNewerAppForTheAppToInstall(t *testing.T) {
	const addr = "127.0.0.1:19615"
	dir := t.TempDir()
	served := filepath.Join(dir, "served.apk")
	installed := filepath.Join(dir, "installed.apk")
	os.WriteFile(served, []byte("home harness app, version B"), 0o644)
	os.WriteFile(installed, []byte("home harness app, version A"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second, AppAPK: served})
	transport.Handle("GET "+domain.AppBinaryRoute, srv.AppBinaryHandler())
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)

	update := filepath.Join(dir, "worker", "update", "home-harness.apk")
	start := func(apk string) (*agent.Agent, context.CancelFunc) {
		actx, acancel := context.WithCancel(ctx)
		a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn, ManagerAddr: addr, PairingToken: pairingToken, Insecure: true,
			IdentityDir: filepath.Join(dir, "identity"), WorkDir: filepath.Join(dir, "work"), Name: "phone", HostFingerprint: "-",
			HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond,
			SelfUpdateDisabled: true, AppAPK: apk, AppUpdateFile: update})
		if err != nil {
			t.Fatal(err)
		}
		go a.Run(actx)
		return a, acancel
	}
	status := func(id domain.NodeID) string {
		var v struct {
			UpdateStatus string `json:"updateStatus"`
		}
		resp, err := http.Get(api.URL + "/nodes/" + string(id))
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(&v)
		return v.UpdateStatus
	}

	a, stop := start(installed)
	waitFor(t, 5*time.Second, func() bool { return status(a.NodeID()) == string(manager.UpdateAvailable) })

	resp, err := http.Post(api.URL+"/nodes/"+string(a.NodeID())+"/update", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST update: %v %v", err, resp.Status)
	}
	resp.Body.Close()
	waitFor(t, 10*time.Second, func() bool {
		b, err := os.ReadFile(update)
		return err == nil && string(b) == "home harness app, version B"
	})

	// The app installs it and restarts the worker, now part of version B.
	stop()
	os.Rename(update, installed)
	b, _ := start(installed)
	waitFor(t, 5*time.Second, func() bool { return status(b.NodeID()) == string(manager.UpdateCurrent) })
}
