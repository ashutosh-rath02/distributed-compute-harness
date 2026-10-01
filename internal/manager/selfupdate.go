package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"

	"home-harness/internal/domain"
)

// hashFile returns the hex-encoded SHA-256 of path's content — same
// encoding convention as internal/mtls.Fingerprint and this project's own
// agent-side twin (internal/agent's hashFile); kept as a small independent
// duplicate rather than a shared package, matching how manager and agent
// deliberately don't share internal code today.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// UpdateStatus is whether, and how, a node can be moved onto the agent
// build this manager serves for its platform.
type UpdateStatus string

const (
	// UpdateCurrent: the node already runs its platform's served build.
	UpdateCurrent UpdateStatus = "current"
	// UpdateAvailable: a different build for the node's platform is
	// loaded and the node can fetch it.
	UpdateAvailable UpdateStatus = "available"
	// UpdateReinstallRequired: the node predates per-platform updates
	// (no domain.FeatureSelfUpdatePath), so it can only download the
	// legacy /agent-binary route — which serves a different platform's
	// build. Updating it would only end in a hash mismatch; it needs one
	// manual reinstall (e.g. a fresh invitation) to join the new protocol.
	UpdateReinstallRequired UpdateStatus = "reinstall-required"
	// UpdateUnknown: nothing to compare — no build loaded for the node's
	// platform, or the node never reported a binary hash.
	UpdateUnknown UpdateStatus = "unknown"
)

// updateTarget decides rec's UpdateStatus and the build it would receive.
// It is platform-aware by construction: a node is only ever compared
// with, or offered, the build for its own reported OS/architecture.
// (Before the catalog, a platform-blind hash comparison could push a
// Windows build onto an Android node; the hash check would pass, since
// the manager named that very file, and the agent could never start
// again.)
func (s *Server) updateTarget(rec *NodeRecord) (UpdateStatus, agentBuild) {
	build, ok := s.agents.forPlatform(rec.Node.Platform.OS, rec.Node.Platform.Architecture)
	if !ok || rec.Node.BinaryHash == "" {
		return UpdateUnknown, agentBuild{}
	}
	if rec.Node.BinaryHash == build.SHA256 {
		return UpdateCurrent, build
	}
	if !rec.hasAgentFeature(domain.FeatureSelfUpdatePath) {
		if primary, _ := s.agents.primary(); primary.platform() != build.platform() {
			return UpdateReinstallRequired, build
		}
	}
	return UpdateAvailable, build
}

// UpdateStatusFor reports rec's UpdateStatus (see updateTarget).
func (s *Server) UpdateStatusFor(rec *NodeRecord) UpdateStatus {
	status, _ := s.updateTarget(rec)
	return status
}

// AgentBinaryHash returns the legacy primary build's hash, or "" if no
// agent builds are loaded. Kept for GET /agent-binary/hash's older
// clients; current code uses the catalog.
func (s *Server) AgentBinaryHash() string {
	b, _ := s.agents.primary()
	return b.SHA256
}

// AgentBinaryPlatform returns the legacy primary build's OS/architecture.
func (s *Server) AgentBinaryPlatform() (goos, arch string) {
	b, _ := s.agents.primary()
	return b.OS, b.Arch
}

// AgentBinaryHandler serves the legacy /agent-binary route: the catalog's
// primary build (agentCatalog.primary), which is what any agent predating
// per-platform updates downloads no matter which platform it runs on —
// updateTarget only ever sends such an agent the primary's own hash.
// Registered on both direct and relay-backed transport listeners
// (cmd/manager/main.go), so a download needs no new public inbound port.
// Unauthenticated deliberately: the real security boundary is the
// authenticated connection the SELF_UPDATE command (naming the exact
// expected hash) arrives over, not this transport.
func (s *Server) AgentBinaryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		build, ok := s.agents.primary()
		if !ok {
			http.NotFound(w, r)
			return
		}
		logAgentDownload(r, build)
		http.ServeFile(w, r, build.Path)
	}
}

// logAgentDownload records each agent-binary download. They are rare (an
// onboarding or an update), and knowing which route and build a node
// actually fetched is the first question when an update doesn't land.
func logAgentDownload(r *http.Request, build agentBuild) {
	log.Printf("manager: serving agent build %s via %s to %s", build.platform(), r.URL.Path, r.RemoteAddr)
}
