package domain

import "time"

// NodeMeta is operator-owned metadata for a node: a display alias and
// free-form labels (future placement selectors). Unlike everything in the
// manifest, the agent never sets or overwrites it.
type NodeMeta struct {
	Alias  string            `json:"alias,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Empty reports whether m carries nothing worth persisting.
func (m NodeMeta) Empty() bool { return m.Alias == "" && len(m.Labels) == 0 }

// AuditLog selects one of three independently capped audit logs, so
// nothing a peer does can evict the operator's security history.
type AuditLog string

const (
	// AuditSecurity holds only actions taken with the operator credential:
	// revocations, invitations, sign-ins, metadata changes, update
	// dispatches, workload submissions. Nothing a peer does can write it,
	// so no peer can evict it.
	AuditSecurity AuditLog = "security"
	// AuditAdmissions holds first admissions (and how each node got in).
	// Separate because a holder of the shared pairing token can mint new
	// identities at will, and must not be able to evict the security log
	// by doing so.
	AuditAdmissions AuditLog = "admissions"
	// AuditNoise holds high-volume, peer-driven entries: rejected
	// registrations and routine reconnects.
	AuditNoise AuditLog = "noise"
)

// AuditEntry is one durable record of something security-relevant.
// Actor says which credential acted ("operator-token",
// "dashboard-session"), or "node"/"anonymous" for peer-driven entries.
type AuditEntry struct {
	Seq    uint64         `json:"seq"`
	Log    AuditLog       `json:"log"`
	Time   time.Time      `json:"time"`
	Kind   string         `json:"kind"`
	NodeID NodeID         `json:"nodeId,omitempty"`
	Actor  string         `json:"actor"`
	Detail map[string]any `json:"detail,omitempty"`
}
