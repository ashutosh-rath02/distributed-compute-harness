package manager

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const joinTestFingerprint = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func newJoinPageServer(t *testing.T, appAPK string) *Server {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s := NewServer(nil, nil, Config{
		Addr: ":7420", Fingerprint: joinTestFingerprint, AppAPK: appAPK,
		PairingToken: "the-permanent-pairing-secret", OperatorToken: testOperatorToken,
	})
	s.OpenJoinWindow(time.Hour) // adding devices is on for these tests
	s.agents = &agentCatalog{builds: []agentBuild{
		{OS: "windows", Arch: "amd64", SHA256: "aaaa1111", Path: write("agent.exe", "windows agent bytes")},
		{OS: "darwin", Arch: "arm64", SHA256: "bbbb2222", Path: write("agent-darwin-arm64", "mac agent bytes")},
		{OS: "linux", Arch: "amd64", SHA256: "cccc3333", Path: write("agent-linux-amd64", "linux agent bytes")},
	}}
	return s
}

func joinGet(t *testing.T, h http.Handler, host, path, userAgent string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	req.Header.Set("User-Agent", userAgent)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestJoinPageShowsTheVisitorsOSFirst(t *testing.T) {
	h := newJoinPageServer(t, "").JoinPageHandler()
	win := joinGet(t, h, "192.168.1.20:7419", "/", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	body := win.Body.String()
	if win.Code != http.StatusOK || !strings.Contains(body, `irm http://192.168.1.20:7419/install/windows.ps1 | iex`) {
		t.Fatalf("windows page: %d\n%s", win.Code, body)
	}
	if strings.Index(body, `id="cmd-windows"`) > strings.Index(body, `id="cmd-macos"`) {
		t.Fatal("a Windows visitor must see the Windows command first")
	}
	mac := joinGet(t, h, "192.168.1.20:7419", "/", "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5)").Body.String()
	if strings.Index(mac, `id="cmd-macos"`) > strings.Index(mac, `id="cmd-windows"`) ||
		!strings.Contains(mac, "curl -fsS http://192.168.1.20:7419/install/macos.sh | bash") {
		t.Fatal("a Mac visitor must see the Mac command first")
	}
	if win.Header().Get("Content-Security-Policy") == "" || win.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("the join page needs its security headers")
	}
}

// The Host header ends up inside PowerShell and bash scripts: anything but
// a plain host:port is refused rather than escaped.
func TestJoinPageRefusesOddHosts(t *testing.T) {
	h := newJoinPageServer(t, "").JoinPageHandler()
	for _, host := range []string{`1.2.3.4:7419"; rm -rf ~; "`, "1.2.3.4", "a b:7419", "$(calc):7419", "x.example:7419/../", "1.2.3.4:74a9"} {
		for _, path := range []string{"/", "/install/windows.ps1", "/install/macos.sh", "/install/linux.sh"} {
			if rec := joinGet(t, h, host, path, "Windows"); rec.Code != http.StatusBadRequest {
				t.Errorf("Host %q %s: got %d, want 400", host, path, rec.Code)
			}
		}
	}
}

// Everything on this port is public on the LAN: none of it may carry a
// credential, and the Windows installer must reach PowerShell 5.1 intact
// (irm | iex: plain ASCII text).
func TestJoinInstallers(t *testing.T) {
	s := newJoinPageServer(t, "")
	h := s.JoinPageHandler()
	win := joinGet(t, h, "192.168.1.20:7419", "/install/windows.ps1", "")
	script := win.Body.String()
	if win.Code != http.StatusOK || !strings.HasPrefix(win.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("windows installer: %d %s", win.Code, win.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		"-pair -manager-fingerprint " + joinTestFingerprint + " -manager-addr-fallback 192.168.1.20:7420",
		`curl.exe -fsS "http://192.168.1.20:7419/agent/windows/amd64"`, `"AAAA1111"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("windows installer lacks %q", want)
		}
	}
	for i, r := range script {
		if r > 127 {
			t.Fatalf("windows installer has a non-ASCII character %q at %d", r, i)
		}
	}
	// The order that makes the printed code the running agent's: replace
	// the binary (after stopping an old one), create the identity, then
	// start it.
	order := []string{"Stop-ScheduledTask", "Move-Item $download $agent", "-pair-code", "Register-ScheduledTask", "Start-ScheduledTask"}
	last := -1
	for _, step := range order {
		i := strings.Index(script, step)
		if i < last {
			t.Fatalf("windows installer: %q comes too early (want order %v)", step, order)
		}
		last = i
	}

	mac := joinGet(t, h, "192.168.1.20:7419", "/install/macos.sh", "").Body.String()
	if !strings.Contains(mac, `url="http://192.168.1.20:7419/agent/darwin/arm64"`) ||
		strings.Index(mac, "-pair-code") > strings.Index(mac, "launchctl bootstrap") {
		t.Fatalf("mac installer must download from the join page and create the identity before starting:\n%s", mac)
	}

	for _, path := range []string{"/", "/install/windows.ps1", "/install/macos.sh", "/install/linux.sh"} {
		body := joinGet(t, h, "192.168.1.20:7419", path, "").Body.String()
		for _, secret := range []string{"the-permanent-pairing-secret", testOperatorToken, dashboardSession(testOperatorToken), "-pairing-token"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s carries %q", path, secret)
			}
		}
	}

	s.cfg.Fingerprint = ""
	if rec := joinGet(t, h, "192.168.1.20:7419", "/install/windows.ps1", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without TLS there is nothing to pin: got %d", rec.Code)
	}
}

func TestJoinPageDownloads(t *testing.T) {
	apk := filepath.Join(t.TempDir(), "base.apk")
	os.WriteFile(apk, []byte("an apk"), 0o644)
	h := newJoinPageServer(t, apk).JoinPageHandler()
	bin := joinGet(t, h, "192.168.1.20:7419", "/agent/windows/amd64", "")
	if bin.Code != http.StatusOK || bin.Body.String() != "windows agent bytes" || bin.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("agent download: %d %q %s", bin.Code, bin.Body.String(), bin.Header().Get("Content-Type"))
	}
	if rec := joinGet(t, h, "192.168.1.20:7419", "/agent/windows/../../etc", ""); rec.Code == http.StatusOK {
		t.Fatal("only catalog builds may be served")
	}
	app := joinGet(t, h, "192.168.1.20:7419", "/app.apk", "")
	if app.Code != http.StatusOK || app.Body.String() != "an apk" ||
		app.Header().Get("Content-Type") != "application/vnd.android.package-archive" || !strings.Contains(app.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("app download: %d %v", app.Code, app.Header())
	}
	if !strings.Contains(joinGet(t, h, "192.168.1.20:7419", "/", "Mozilla/5.0 (Linux; Android 15)").Body.String(), `href="/app.apk"`) {
		t.Fatal("an Android visitor must be offered the app")
	}
	noApp := newJoinPageServer(t, "").JoinPageHandler()
	if rec := joinGet(t, noApp, "192.168.1.20:7419", "/app.apk", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("no app configured: got %d", rec.Code)
	}
}
