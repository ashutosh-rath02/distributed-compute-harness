package manager

import "home-harness/internal/joinscript"

// JoinInfo is what a new node needs to onboard itself: the manager's own
// TLS fingerprint and pairing token, handed out over the manager's
// loopback API (GET /join-info, api.go) rather than requiring an operator
// to copy them by hand from the manager's startup log — see
// cmd/harnessctl's `join` command. Same trust model as every other
// operator endpoint here: same-machine access to the manager's API is
// already fully privileged (and guarded against browsers, apiguard.go).
type JoinInfo struct {
	Fingerprint  string `json:"fingerprint"`
	PairingToken string `json:"pairingToken"`
	Insecure     bool   `json:"insecure"`
	// AgentBinaries is the loaded agent-build catalog. A join script
	// downloads the build for its own target platform
	// (joinscript.TargetPlatform) and verifies it against that build's
	// SHA-256 rather than trusting the bootstrap TLS connection alone —
	// the script has no fingerprint to pin yet, so its download runs with
	// certificate validation off and the hash is what authenticates the
	// bytes. This list replaced single agentBinaryAvailable /
	// agentBinarySha256 fields on purpose: an older harnessctl would have
	// applied that one hash to whatever platform it was asked to onboard,
	// producing an Android script that verifies (and installs) a Windows
	// binary. Without those fields such a CLI fails safely instead.
	AgentBinaries  []AgentBinaryView `json:"agentBinaries"`
	RelayAvailable bool              `json:"relayAvailable"`
	RelayAddr      string            `json:"relayAddr,omitempty"`
	RelayToken     string            `json:"relayToken,omitempty"`
}

// JoinInfo reports what an operator needs to onboard a new node. Insecure
// is derived from Fingerprint being empty rather than a separate Config
// field — Fingerprint is only ever populated in non-insecure mode
// (cmd/manager/main.go), so the two are equivalent.
func (s *Server) JoinInfo() JoinInfo {
	return JoinInfo{
		Fingerprint:    s.cfg.Fingerprint,
		PairingToken:   s.cfg.PairingToken,
		Insecure:       s.cfg.Fingerprint == "",
		AgentBinaries:  s.agents.views(),
		RelayAvailable: s.cfg.RelayAddr != "" && s.cfg.RelayToken != "",
		RelayAddr:      s.cfg.RelayAddr,
		RelayToken:     s.cfg.RelayToken,
	}
}

// joinBuild returns the catalog build an onboarding script for platform
// ("windows", "android") installs, if one is loaded.
func (s *Server) joinBuild(platform string) (agentBuild, bool) {
	goos, arch, ok := joinscript.TargetPlatform(platform)
	if !ok {
		return agentBuild{}, false
	}
	return s.agents.forPlatform(goos, arch)
}

// scriptInfo is the joinscript.Info for onboarding platform with this
// manager's credentials and the matching catalog build.
func (s *Server) scriptInfo(platform string) joinscript.Info {
	build, ok := s.joinBuild(platform)
	return joinscript.Info{
		Fingerprint:          s.cfg.Fingerprint,
		PairingToken:         s.cfg.PairingToken,
		Insecure:             s.cfg.Fingerprint == "",
		AgentBinaryAvailable: ok,
		AgentBinarySHA256:    build.SHA256,
		AgentBinaryPath:      build.downloadPath(),
		RelayAvailable:       s.cfg.RelayAddr != "" && s.cfg.RelayToken != "",
		RelayAddr:            s.cfg.RelayAddr,
		RelayToken:           s.cfg.RelayToken,
	}
}
