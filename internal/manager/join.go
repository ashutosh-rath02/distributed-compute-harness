package manager

// JoinInfo is what a new node needs to onboard itself: the manager's own
// TLS fingerprint and pairing token, handed out over the manager's
// loopback API (GET /join-info, api.go) rather than requiring an operator
// to copy them by hand from the manager's startup log — see
// cmd/harnessctl's `join` command. Same trust model as every other
// unauthenticated-but-loopback-scoped endpoint here (GET
// /agent-binary/hash, POST /nodes/{id}/commands): same-machine access to
// the manager's API is already fully privileged, so this adds nothing new.
type JoinInfo struct {
	Fingerprint          string `json:"fingerprint"`
	PairingToken         string `json:"pairingToken"`
	Insecure             bool   `json:"insecure"`
	AgentBinaryAvailable bool   `json:"agentBinaryAvailable"`
	// AgentBinarySHA256 lets the join-script verify its own download
	// against the same hash self-update already trusts (NeedsUpdate),
	// rather than trusting the bootstrap TLS connection alone — the
	// join-script has no fingerprint to pin against yet (that's the whole
	// point of this endpoint), so its download step necessarily runs with
	// certificate validation off; the hash check is what actually
	// authenticates the bytes, the same reasoning v4's /agent-binary
	// endpoint already documents. Empty iff AgentBinaryAvailable is false.
	AgentBinarySHA256 string `json:"agentBinarySha256"`
	RelayAvailable    bool   `json:"relayAvailable"`
	RelayAddr         string `json:"relayAddr,omitempty"`
	RelayToken        string `json:"relayToken,omitempty"`
}

// JoinInfo reports what an operator needs to onboard a new node. Insecure
// is derived from Fingerprint being empty rather than a separate Config
// field — Fingerprint is only ever populated in non-insecure mode
// (cmd/manager/main.go), so the two are equivalent.
func (s *Server) JoinInfo() JoinInfo {
	return JoinInfo{
		Fingerprint:          s.cfg.Fingerprint,
		PairingToken:         s.cfg.PairingToken,
		Insecure:             s.cfg.Fingerprint == "",
		AgentBinaryAvailable: s.agentBinaryHash != "",
		AgentBinarySHA256:    s.agentBinaryHash,
		RelayAvailable:       s.cfg.RelayAddr != "" && s.cfg.RelayToken != "",
		RelayAddr:            s.cfg.RelayAddr,
		RelayToken:           s.cfg.RelayToken,
	}
}
