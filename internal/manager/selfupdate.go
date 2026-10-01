package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
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

// NeedsUpdate reports whether rec's last-reported BinaryHash differs from
// the binary this manager currently serves. Always false if self-update is
// disabled (AgentBinaryPath unset/unreadable — agentBinaryHash empty) or if
// the node has never reported a hash at all (a pre-v4-part-2 agent) — in
// both cases there is nothing to compare against, not a mismatch to act on.
func (s *Server) NeedsUpdate(rec *NodeRecord) bool {
	if s.agentBinaryHash == "" || rec.Node.BinaryHash == "" {
		return false
	}
	return rec.Node.BinaryHash != s.agentBinaryHash
}

// AgentBinaryHash returns the manager's currently-served agent binary hash,
// or "" if self-update is disabled.
func (s *Server) AgentBinaryHash() string {
	return s.agentBinaryHash
}

// AgentBinaryPlatform returns the currently-served agent binary's
// best-effort detected OS/architecture (both "" if self-update is
// disabled or the format wasn't recognized) — see binaryplatform.go.
func (s *Server) AgentBinaryPlatform() (goos, arch string) {
	return s.agentBinaryOS, s.agentBinaryArch
}

// AgentBinaryHandler serves cfg.AgentBinaryPath's raw bytes — registered on
// both direct and relay-backed transport listeners (cmd/manager/main.go),
// so an agent's self-update download needs no new public inbound port.
// Unauthenticated deliberately: the real security boundary is the already
// mTLS-authenticated connection the SELF_UPDATE command (naming the exact
// expected hash) arrives over, not this transport — anyone who could reach
// this port could already do everything a full system.execute grants.
func (s *Server) AgentBinaryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AgentBinaryPath == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, s.cfg.AgentBinaryPath)
	}
}
