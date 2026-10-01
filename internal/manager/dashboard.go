package manager

import (
	_ "embed"
	"net/http"

	"home-harness/internal/joinscript"
)

// dashboardHTML is the manager's local web dashboard (v5 part 2): a
// single self-contained page (no build step, no CDN dependencies — this
// project is local-first by design) showing known nodes live via the
// existing GET /events SSE stream, and a friendlier way to generate the
// onboarding script than remembering `harnessctl join` flags.
//
//go:embed dashboard.html
var dashboardHTML []byte

// apiGetDashboard serves the dashboard page itself. Registered on "GET
// /{$}" (Go 1.22+'s exact-root-only pattern) so it only matches "/",
// leaving every other path to its own handler.
func (s *Server) apiGetDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

// apiGetJoinScript is GET /join-info's sibling for the dashboard's "add a
// device" form: builds the exact same onboarding script cmd/harnessctl's
// `join` command prints, via the shared internal/joinscript package (the
// one place that logic lives — see joinscript's doc comment), so the CLI
// and the dashboard can never drift apart on script format.
func (s *Server) apiGetJoinScript(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("addr")
	mode := joinscript.Mode(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = joinscript.ModeLAN
	}
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = "windows"
	}

	script, err := joinscript.BuildMode(mode, addr, platform, s.scriptInfo(platform))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(script))
}
