package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"home-harness/internal/manager"
	relayproto "home-harness/internal/relay"
	relaytransport "home-harness/internal/transport/relay"
	"home-harness/internal/transport/ws"
)

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestOneManagerOnboardsEveryLoadedPlatform is the catalog's reason to
// exist: a single manager (as on the phone) hands out Windows *and*
// Android invitations at once, each installing exactly its own build.
func TestOneManagerOnboardsEveryLoadedPlatform(t *testing.T) {
	const addr = "127.0.0.1:19505"
	windowsPath, windowsHash := writeDummyAgentBinary(t, []byte("windows build bytes"))
	androidPath, androidHash := writeDummyAgentBinary(t, []byte("android build bytes"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, EnrollmentTTL: time.Minute,
		// Android listed first on purpose: the legacy route must still
		// serve the Windows build (see below).
		AgentBinaries: []manager.AgentBinary{
			{OS: "linux", Arch: "arm64", Path: androidPath},
			{OS: "windows", Arch: "amd64", Path: windowsPath},
		},
	})
	transport.Handle("/agent-binary", srv.AgentBinaryHandler())
	transport.Handle("GET /agent-binaries/{os}/{arch}", srv.AgentBinariesHandler())
	transport.Handle("/enroll/", srv.EnrollmentHandler())
	go srv.Run(ctx)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()

	for _, tc := range []struct {
		platform, hash, bytes string
	}{
		{"windows", strings.ToUpper(windowsHash), "windows build bytes"},
		{"android", androidHash, "android build bytes"},
	} {
		var invite struct {
			URL string `json:"url"`
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := http.Post(api.URL+"/enrollments", "application/json",
				bytes.NewReader([]byte(`{"addr":"`+addr+`","platform":"`+tc.platform+`"}`)))
			if err != nil {
				t.Fatalf("create %s invitation: %v", tc.platform, err)
			}
			json.NewDecoder(resp.Body).Decode(&invite)
			resp.Body.Close()
			if resp.StatusCode == http.StatusCreated {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("create %s invitation: status %d", tc.platform, resp.StatusCode)
			}
			time.Sleep(20 * time.Millisecond)
		}
		_, setup := get(t, invite.URL+"/setup")
		if !strings.Contains(setup, tc.hash) {
			t.Errorf("%s setup script does not verify its own build's hash %s:\n%s", tc.platform, tc.hash, setup)
		}
		if code, body := get(t, invite.URL+"/agent-binary"); code != http.StatusOK || body != tc.bytes {
			t.Errorf("%s invitation served %d %q, want its own build %q", tc.platform, code, body, tc.bytes)
		}
	}

	if _, body := get(t, "http://"+addr+"/agent-binaries/linux/arm64"); body != "android build bytes" {
		t.Errorf("/agent-binaries/linux/arm64 served %q", body)
	}
	if code, _ := get(t, "http://"+addr+"/agent-binaries/darwin/arm64"); code != http.StatusNotFound {
		t.Errorf("expected 404 for an unloaded platform, got %d", code)
	}
	if code, _ := get(t, "http://"+addr+"/agent-binaries/..%2f..%2fwindows/amd64"); code == http.StatusOK {
		t.Error("a traversal-shaped platform must not resolve to a build")
	}
	// Pre-catalog agents only know /agent-binary, and every one deployed
	// is a Windows build: it must get Windows bytes regardless of order.
	if _, body := get(t, "http://"+addr+"/agent-binary"); body != "windows build bytes" {
		t.Errorf("legacy /agent-binary served %q, want the windows build", body)
	}

	var catalog []map[string]string
	_, listing := get(t, api.URL+"/agent-binaries")
	json.Unmarshal([]byte(listing), &catalog)
	if len(catalog) != 2 || catalog[0]["os"] != "linux" || catalog[1]["sha256"] != windowsHash {
		t.Errorf("GET /agent-binaries: unexpected catalog %s", listing)
	}
}

// TestAgentBuildDownloadThroughRelay proves the per-platform route works
// on the relay-backed listener too — that's how an off-LAN node
// self-updates.
func TestAgentBuildDownloadThroughRelay(t *testing.T) {
	const session = "catalog-relay-session"
	relayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go relayproto.NewServer(5*time.Second).Serve(ctx, relayListener)

	androidPath, _ := writeDummyAgentBinary(t, []byte("android build via relay"))
	managerTransport := relaytransport.New(session)
	srv := manager.NewServer(managerTransport, nil, manager.Config{
		Addr: relayListener.Addr().String(), PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second,
		AgentBinaries: []manager.AgentBinary{{OS: "linux", Arch: "arm64", Path: androidPath}},
	})
	managerTransport.Handle("GET /agent-binaries/{os}/{arch}", srv.AgentBinariesHandler())
	go srv.Run(ctx)

	client := relaytransport.NewClient(session).HTTPClient(relayListener.Addr().String())
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://manager/agent-binaries/linux/arm64")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "android build via relay" {
				t.Fatalf("relay download served %q", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay download: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A macOS invitation can't know the Mac's CPU, so its script carries every
// darwin build and downloads its own architecture's from the invitation.
func TestMacOSInvitationServesEachArchitecture(t *testing.T) {
	const addr = "127.0.0.1:19515"
	armPath, armHash := writeDummyAgentBinary(t, []byte("apple silicon build"))
	intelPath, intelHash := writeDummyAgentBinary(t, []byte("intel mac build"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 2 * time.Second, EnrollmentTTL: time.Minute,
		AgentBinaries: []manager.AgentBinary{
			{OS: "darwin", Arch: "arm64", Path: armPath},
			{OS: "darwin", Arch: "amd64", Path: intelPath},
		},
	})
	transport.Handle("/enroll/", srv.EnrollmentHandler())
	go srv.Run(ctx)
	api := httptest.NewServer(srv.NewHTTPHandler())
	defer api.Close()

	var invite struct {
		URL string `json:"url"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Post(api.URL+"/enrollments", "application/json", bytes.NewReader([]byte(`{"addr":"`+addr+`","platform":"macos"}`)))
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(&invite)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("create macOS invitation: %d", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, page := get(t, invite.URL); !strings.Contains(page, "| bash") {
		t.Fatalf("macOS invitation page should offer the curl | bash one-liner:\n%s", page)
	}
	_, setup := get(t, invite.URL+"/setup")
	for _, want := range []string{armHash, intelHash, "/agent-binary/arm64", "/agent-binary/amd64", "launchctl"} {
		if !strings.Contains(setup, want) {
			t.Errorf("macOS setup script missing %q:\n%s", want, setup)
		}
	}
	if _, body := get(t, invite.URL+"/agent-binary/arm64"); body != "apple silicon build" {
		t.Errorf("arm64 download served %q", body)
	}
	if _, body := get(t, invite.URL+"/agent-binary/amd64"); body != "intel mac build" {
		t.Errorf("amd64 download served %q", body)
	}
	if code, _ := get(t, invite.URL+"/agent-binary"); code != http.StatusNotFound {
		t.Errorf("a Unix invitation must not serve an arch-less download, got %d", code)
	}
}
