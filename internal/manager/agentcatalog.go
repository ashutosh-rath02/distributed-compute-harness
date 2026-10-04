package manager

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"home-harness/internal/domain"
)

// AgentBinary names one agent build the manager serves. OS/Arch are
// optional: empty means "read it from the file's header"
// (detectBinaryPlatform). Give them explicitly only for a file whose
// format can't be detected; if the header *is* recognized and disagrees,
// the catalog refuses to load rather than serve the wrong build.
type AgentBinary struct {
	OS   string
	Arch string
	Path string
}

// agentBuild is one loaded catalog entry (or the Android app, which
// agents updated with it download from route).
type agentBuild struct {
	OS, Arch, Path, SHA256 string
	route                  string
}

func (b agentBuild) platform() string { return b.OS + "/" + b.Arch }

// downloadPath is the agent-transport route serving this build.
func (b agentBuild) downloadPath() string {
	if b.route != "" {
		return b.route
	}
	return "/agent-binaries/" + b.OS + "/" + b.Arch
}

// loadApp hashes the Android app the manager offers to agents that update
// with it (nil if none is configured or it can't be read).
func loadApp(path string) *agentBuild {
	if path == "" {
		return nil
	}
	hash, err := hashFile(path)
	if err != nil {
		log.Printf("manager: app %s: %v (apps won't be offered updates)", path, err)
		return nil
	}
	return &agentBuild{OS: "android", Arch: "app", Path: path, SHA256: hash, route: domain.AppBinaryRoute}
}

// AppBinaryHandler serves the Android app at domain.AppBinaryRoute on the
// agent-facing listeners, for agents updated with the app. Like the agent
// builds, unauthenticated: the SELF_UPDATE naming its exact hash arrives
// over the authenticated connection, and Android installs only an APK
// signed with the installed app's key.
func (s *Server) AppBinaryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.app == nil {
			http.NotFound(w, r)
			return
		}
		logAgentDownload(r, *s.app)
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
		http.ServeFile(w, r, s.app.Path)
	}
}

// agentCatalog is the set of agent builds this manager serves, keyed by
// GOOS/GOARCH — one per platform, so onboarding and self-update always
// hand a node the build for its own platform. There is deliberately no
// wildcard entry: serving one build to "any" platform is exactly how a
// Windows binary once could be pushed onto an Android node and brick it.
type agentCatalog struct {
	builds []agentBuild // in configuration order
}

// BuildAgentCatalog hashes and identifies each binary once. It errors on
// an unreadable file, an undetectable platform with none given, an
// explicit platform contradicting the header, or two builds for the same
// platform. An empty list is a valid, empty catalog (self-update and
// binary onboarding disabled).
func BuildAgentCatalog(binaries []AgentBinary) (*agentCatalog, error) {
	c := &agentCatalog{}
	seen := make(map[string]string)
	for _, b := range binaries {
		hash, err := hashFile(b.Path)
		if err != nil {
			return nil, fmt.Errorf("agent binary %q: %w", b.Path, err)
		}
		goos, arch := detectBinaryPlatform(b.Path)
		switch {
		case b.OS == "" && b.Arch == "":
			if goos == "" || arch == "" {
				return nil, fmt.Errorf("agent binary %q: could not detect its platform from the file header; give it explicitly as os/arch=%s", b.Path, b.Path)
			}
		case b.OS == "" || b.Arch == "":
			return nil, fmt.Errorf("agent binary %q: give both os and arch, or neither", b.Path)
		default:
			if goos != "" && (goos != b.OS || (arch != "" && arch != b.Arch)) {
				return nil, fmt.Errorf("agent binary %q: declared %s/%s but its header says %s/%s", b.Path, b.OS, b.Arch, goos, arch)
			}
			goos, arch = b.OS, b.Arch
		}
		build := agentBuild{OS: goos, Arch: arch, Path: b.Path, SHA256: hash}
		if prev, dup := seen[build.platform()]; dup {
			return nil, fmt.Errorf("two agent binaries for %s: %q and %q", build.platform(), prev, b.Path)
		}
		seen[build.platform()] = b.Path
		c.builds = append(c.builds, build)
	}
	return c, nil
}

// ParseAgentBinaryFlag parses one -agent-binary value: "path" (platform
// detected from the file) or "os/arch=path".
func ParseAgentBinaryFlag(value string) (AgentBinary, error) {
	if platform, path, ok := strings.Cut(value, "="); ok {
		goos, arch, ok := strings.Cut(platform, "/")
		if !ok || goos == "" || arch == "" || path == "" {
			return AgentBinary{}, fmt.Errorf("invalid -agent-binary %q (want path or os/arch=path)", value)
		}
		return AgentBinary{OS: goos, Arch: arch, Path: path}, nil
	}
	if value == "" {
		return AgentBinary{}, fmt.Errorf("empty -agent-binary")
	}
	return AgentBinary{Path: value}, nil
}

// Describe lists each build as "os/arch <- path", for startup checks.
func (c *agentCatalog) Describe() []string {
	var out []string
	for _, b := range c.builds {
		out = append(out, b.platform()+" <- "+b.Path)
	}
	return out
}

func (c *agentCatalog) empty() bool { return c == nil || len(c.builds) == 0 }

// forPlatform returns the build for goos/arch, if loaded.
func (c *agentCatalog) forPlatform(goos, arch string) (agentBuild, bool) {
	if c == nil {
		return agentBuild{}, false
	}
	for _, b := range c.builds {
		if b.OS == goos && b.Arch == arch {
			return b, true
		}
	}
	return agentBuild{}, false
}

// primary is the build behind the legacy /agent-binary route — the only
// route an agent predating domain.FeatureSelfUpdatePath ever downloads
// from. It must not depend on flag order (a glob puts agent-linux-arm64
// before agent.exe), so windows/amd64 wins when loaded: every agent
// deployed before the catalog existed is a Windows build, and this keeps
// their one-time upgrade onto the new protocol working.
func (c *agentCatalog) primary() (agentBuild, bool) {
	if c.empty() {
		return agentBuild{}, false
	}
	if b, ok := c.forPlatform("windows", "amd64"); ok {
		return b, true
	}
	return c.builds[0], true
}

// AgentBinaryView is the JSON shape of one catalog entry.
type AgentBinaryView struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	SHA256       string `json:"sha256"`
	Path         string `json:"path"`
}

func (c *agentCatalog) views() []AgentBinaryView {
	out := []AgentBinaryView{}
	if c == nil {
		return out
	}
	for _, b := range c.builds {
		out = append(out, AgentBinaryView{OS: b.OS, Architecture: b.Arch, SHA256: b.SHA256, Path: b.downloadPath()})
	}
	return out
}

// AgentBinariesHandler serves GET /agent-binaries/{os}/{arch} on the agent
// transports. The file served always comes from the catalog lookup — the
// URL's values only select among configured entries and never become part
// of a filesystem path. Unauthenticated for the same reason as
// AgentBinaryHandler: agents verify every download against the hash named
// over their authenticated connection.
func (s *Server) AgentBinariesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		build, ok := s.agents.forPlatform(r.PathValue("os"), r.PathValue("arch"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		logAgentDownload(r, build)
		http.ServeFile(w, r, build.Path)
	}
}
