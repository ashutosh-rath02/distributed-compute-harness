package domain

import "time"

// FeatureFailover means the agent follows a standby manager (roadmap
// item 17): it keeps the manager addresses and the failover term the
// active manager sends, sends that term when it registers, and on losing
// its manager tries the known addresses too — accepting only a manager
// whose term is at least the one it last saw. Agents without it keep
// working against the address they were set up with.
const FeatureFailover = "failover.v1"

// StandbyPair is what a manager keeps about its partner in a
// primary/standby pair: the shared secret that authenticates the standby
// link in both directions, and the partner's agent-facing address. It is
// stored in the manager database, so the standby's copy carries it too;
// after a promotion PeerAddr names the old primary. Secret is never shown
// by the operator API.
type StandbyPair struct {
	Secret   string    `json:"secret"`
	PeerAddr string    `json:"peerAddr"`
	PeerName string    `json:"peerName,omitempty"`
	AddedAt  time.Time `json:"addedAt"`
}

// EventStandby: the standby changed (added, removed, first copy, stepped
// down). Data: what happened.
const EventStandby EventType = "standby.updated"
