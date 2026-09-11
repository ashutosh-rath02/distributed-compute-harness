package domain

import "context"

// Discoverer finds a reachable manager address without requiring a
// hardcoded configuration, so the discovery mechanism (LAN multicast
// today; mesh, relay, manual pairing, or QR-code pairing later) can be
// swapped without changing agent logic (v1.md §4.1). A concrete
// implementation is free to block until it finds something or ctx is
// canceled/times out.
type Discoverer interface {
	Discover(ctx context.Context) (managerAddr string, err error)
}
