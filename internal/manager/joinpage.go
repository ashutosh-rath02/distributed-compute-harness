package manager

import (
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"home-harness/internal/joinscript"
)

// The join page (cmd/manager -join-addr, plain HTTP on the LAN) is what a
// device opens to add itself: http://<manager-ip>:7419 shows the one
// command for that device's OS, whose installer starts the agent in
// pairing mode (agent -pair). The device is admitted only when the
// operator approves it on the dashboard after comparing the pairing code,
// so this port serves nothing secret: the page, installers, agent builds
// and the Android app. Anyone on the LAN could fetch these; what they
// can't do is get in without an Approve.
//
// Plain HTTP, so no certificate warning on every device — and what that
// costs, stated plainly: the pairing code protects admission and pinning
// (which manager the device trusts, which device the manager lets in; a
// machine in the middle makes the codes differ: protocol.PairingCode). It
// does NOT protect what is downloaded here. Someone able to intercept the
// LAN at install time can serve a different installer or APK, which runs
// (irm | iex, curl | bash) and can install the real agent too, so the
// codes still match; the installers' hash checks only catch corruption,
// since the hash comes over the same channel. That is the same exposure
// as accepting a self-signed certificate on first visit. Add devices only
// on a network you trust, and leave -join-addr off when not adding any.

// JoinPageHandler serves the join page and what it links to.
func (s *Server) JoinPageHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.joinPage)
	mux.HandleFunc("GET /install/windows.ps1", s.joinInstaller("windows"))
	mux.HandleFunc("GET /install/macos.sh", s.joinInstaller("macos"))
	mux.HandleFunc("GET /install/linux.sh", s.joinInstaller("linux"))
	mux.HandleFunc("GET /agent/{os}/{arch}", s.joinAgentBinary)
	mux.HandleFunc("GET /app.apk", s.joinAppAPK)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		// Closed (joinwindow.go): nothing to download, only a note on how
		// to open it.
		if !s.joinWindowOpen() {
			if r.URL.Path != "/" {
				http.Error(w, "Adding devices is switched off on the manager. Open \"Add a device\" on its dashboard, then try again.", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, joinClosedPage)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// joinBase is the page's own origin as the device reached it — the only
// address known to work from that device — or "" if the Host header isn't
// a plain host:port (it ends up inside shell and PowerShell scripts).
func joinBase(r *http.Request) (base, host string) {
	if joinscript.ValidateAddress(r.Host) != nil {
		return "", ""
	}
	h, _, _ := net.SplitHostPort(r.Host)
	return "http://" + r.Host, h
}

// agentPort is the port agents connect to (from -addr).
func (s *Server) agentPort() string {
	if _, port, err := net.SplitHostPort(s.cfg.Addr); err == nil && port != "" {
		return port
	}
	return "7420"
}

func (s *Server) joinInstaller(platform string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base, host := joinBase(r)
		if base == "" {
			http.Error(w, "open this page by the manager's IP address and port (e.g. http://192.168.1.20:7419)", http.StatusBadRequest)
			return
		}
		p := joinscript.PairingInfo{
			Fingerprint:  s.cfg.Fingerprint,
			FallbackAddr: net.JoinHostPort(host, s.agentPort()),
			ArchBuilds:   s.archBuilds(platform, func(b agentBuild) string { return base + "/agent/" + b.OS + "/" + b.Arch }),
		}
		if b, ok := s.joinBuild("windows"); ok {
			p.WindowsURL, p.WindowsSHA256 = base+"/agent/"+b.OS+"/"+b.Arch, b.SHA256
		}
		script, err := joinscript.BuildPairing(platform, p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(script))
	}
}

func (s *Server) joinAgentBinary(w http.ResponseWriter, r *http.Request) {
	build, ok := s.agents.forPlatform(r.PathValue("os"), r.PathValue("arch"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	logAgentDownload(r, build)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, build.Path)
}

func (s *Server) joinAppAPK(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AppAPK == "" {
		http.NotFound(w, r)
		return
	}
	log.Printf("manager: app download by %s", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="home-harness.apk"`)
	http.ServeFile(w, r, s.cfg.AppAPK)
}

// joinOS guesses the visiting device's OS from its User-Agent (only to
// choose which instructions to show first).
func joinOS(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "android"):
		return "android"
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"):
		return "ios"
	case strings.Contains(ua, "windows"):
		return "windows"
	case strings.Contains(ua, "macintosh"), strings.Contains(ua, "mac os x"):
		return "macos"
	case strings.Contains(ua, "linux"), strings.Contains(ua, "x11"):
		return "linux"
	}
	return ""
}

type joinCommand struct {
	OS, Title, Steps, Command string
}

func (s *Server) joinPage(w http.ResponseWriter, r *http.Request) {
	base, _ := joinBase(r)
	if base == "" {
		http.Error(w, "open this page by the manager's IP address and port (e.g. http://192.168.1.20:7419)", http.StatusBadRequest)
		return
	}
	cmds := []joinCommand{
		{OS: "windows", Title: "Windows", Steps: "Copy this, press Win+R, paste it and press Enter (or paste it into PowerShell):",
			Command: `powershell -NoExit -ExecutionPolicy Bypass -Command "irm ` + base + `/install/windows.ps1 | iex"`},
		{OS: "macos", Title: "Mac", Steps: "Open Terminal, paste this and press Enter:",
			Command: "curl -fsS " + base + "/install/macos.sh | bash"},
		{OS: "linux", Title: "Linux", Steps: "In a terminal, run:",
			Command: "curl -fsS " + base + "/install/linux.sh | bash"},
	}
	detected := joinOS(r.UserAgent())
	// The detected OS first, the rest below.
	for i, c := range cmds {
		if c.OS == detected && i > 0 {
			cmds[0], cmds[i] = cmds[i], cmds[0]
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	joinPageTemplate.Execute(w, map[string]any{
		"Commands": cmds, "Detected": detected, "HasApp": s.cfg.AppAPK != "",
		"Ready": s.cfg.Fingerprint != "" && len(s.agents.builds) > 0,
	})
}

const joinClosedPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Adding devices is off - Home Harness</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:0 auto;padding:2rem 1rem;line-height:1.5;color:#1f2328;background:#fff}
h1{font-size:1.4rem}.note{color:#59636e}@media (prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}.note{color:#9198a1}}</style></head>
<body><h1>Adding devices is switched off</h1>
<p>On the manager, open its dashboard and choose <strong>Add a device</strong>. It stays open for 15 minutes; then reload this page.</p>
<p class="note">Devices that already joined keep working: this only stops new ones.</p></body></html>`

var joinPageTemplate = template.Must(template.New("join").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Add this device - Home Harness</title>
<style>
body{font-family:system-ui,sans-serif;max-width:44rem;margin:0 auto;padding:1.5rem 1rem;color:#1f2328;background:#fff}
h1{font-size:1.5rem;margin:.2rem 0 1rem}h2{font-size:1.1rem;margin:1.6rem 0 .4rem}
.card{border:1px solid #d0d7de;border-radius:.6rem;padding:1rem;margin:.8rem 0}
.first{border:2px solid #2f81f7}
pre{white-space:pre-wrap;word-break:break-all;background:#f6f8fa;padding:.8rem;border-radius:.4rem;font-size:.9rem;user-select:all;margin:.5rem 0}
button,a.button{display:inline-block;font:inherit;padding:.55rem 1rem;border-radius:.4rem;border:0;background:#2f81f7;color:#fff;text-decoration:none;cursor:pointer}
.note{color:#59636e;font-size:.92rem}
ol{padding-left:1.3rem}li{margin:.3rem 0}
@media (prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}.card{border-color:#30363d}pre{background:#161b22}.note{color:#9198a1}}
</style></head><body>
<h1>Add this device to Home Harness</h1>
{{if not .Ready}}<p class="card">This manager isn't ready to add devices yet (no agent builds loaded, or it runs without TLS).</p>{{end}}
{{if eq .Detected "android"}}<div class="card first"><h2 style="margin-top:0">Android phone or tablet</h2>
{{if .HasApp}}<ol><li><a class="button" href="/app.apk">Download the Home Harness app</a></li>
<li>Open the download and install it (allow your browser to install apps if asked).</li>
<li>Open the app and choose <strong>Use this device as a worker</strong>.</li>
<li>Where you manage Home Harness (the app or the dashboard), approve the device. Check that the code there is the same.</li></ol>
{{else}}<p>This manager doesn't offer the Android app. Install the Home Harness app on this device and choose <strong>Use this device as a worker</strong>.</p>{{end}}</div>{{end}}
{{if eq .Detected "ios"}}<p class="card first">iPhones and iPads can't run a Home Harness worker.</p>{{end}}
{{range $i, $c := .Commands}}<div class="card{{if and (eq $i 0) (eq $c.OS $.Detected)}} first{{end}}">
<h2 style="margin-top:0">{{$c.Title}}</h2>
<p>{{$c.Steps}}</p>
<pre id="cmd-{{$c.OS}}">{{$c.Command}}</pre>
<button type="button" data-copy="cmd-{{$c.OS}}">Copy</button>
<p class="note">It installs the agent for your user (no admin needed), keeps it running, and prints a pairing code.
Then approve this device where you manage Home Harness (the app, or the dashboard). Check that the code there is the same.</p>
</div>{{end}}
<p class="note">Your phone and this device must be on the same Wi-Fi.</p>
<script>
document.addEventListener("click", function (e) {
  var b = e.target.closest("button[data-copy]");
  if (!b) return;
  var el = document.getElementById(b.getAttribute("data-copy"));
  var range = document.createRange(); range.selectNodeContents(el);
  var sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(range);
  var ok = false; try { ok = document.execCommand("copy"); } catch (err) {}
  b.textContent = ok ? "Copied" : "Select and copy it";
});
</script>
</body></html>`))
