package domain

import "time"

// RevokedNode records an operator's decision to permanently refuse one
// node identity. Since a node's persistent key is its reconnect
// credential, this — not the pairing token — is what takes back an
// admission. Name/Hostname/Platform are a snapshot from the node's last
// manifest, kept only so an operator can still recognize which device an
// entry was after the node itself has been forgotten.
type RevokedNode struct {
	NodeID    NodeID    `json:"nodeId"`
	Name      string    `json:"name,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
	Platform  Platform  `json:"platform"`
	RevokedAt time.Time `json:"revokedAt"`
}
