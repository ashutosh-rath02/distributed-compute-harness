package manager

import (
	"fmt"

	"home-harness/internal/domain"
)

// nodeFits reports whether rec currently satisfies capability and req, and
// if not, a human-readable reason.
//
// The capability check runs first, before the req.IsEmpty() early-return
// below — a workload with no resource requirements at all must still be
// filtered by capability, so this can't be folded into (or skipped by) the
// resource-requirement short-circuit.
//
// A node that hasn't heartbeated yet has a zero-value LastMetrics — its
// CPUPercent and MemoryAvailableBytes read as 0, which looks idle rather
// than unknown. Any requirement that reads live metrics (MinMemoryBytes,
// MaxCPUPercent) treats that as ineligible rather than trusting a zero
// value nobody actually reported. MinCPUCores reads static Resources set
// at registration, which is available immediately, so it isn't affected.
func nodeFits(rec *NodeRecord, capability domain.CapabilityName, req domain.ResourceRequirements) (bool, string) {
	if !rec.HasCapability(capability) {
		return false, fmt.Sprintf("does not declare capability %q", capability)
	}

	if req.IsEmpty() {
		return true, ""
	}

	needsLiveMetrics := req.MinMemoryBytes > 0 || req.MaxCPUPercent > 0
	if needsLiveMetrics && rec.LastMetrics.LastHeartbeat.IsZero() {
		return false, "no heartbeat received yet, live resource state unknown"
	}

	if req.MinCPUCores > 0 {
		cores, ok := domain.StaticCapacity(rec.Resources, domain.ResourceCPUCores)
		if !ok || cores < req.MinCPUCores {
			return false, fmt.Sprintf("requires %.2f CPU cores, node declares %.2f", req.MinCPUCores, cores)
		}
	}

	if req.MinMemoryBytes > 0 && rec.LastMetrics.MemoryAvailableBytes < req.MinMemoryBytes {
		return false, fmt.Sprintf("requires %d bytes available memory, node has %d", req.MinMemoryBytes, rec.LastMetrics.MemoryAvailableBytes)
	}

	if req.MaxCPUPercent > 0 && rec.LastMetrics.CPUPercent > req.MaxCPUPercent {
		return false, fmt.Sprintf("requires CPU load under %.1f%%, node is at %.1f%%", req.MaxCPUPercent, rec.LastMetrics.CPUPercent)
	}

	return true, ""
}

// selectNode picks the best candidate satisfying req: the one with the
// most available memory (the only live, absolute resource signal that
// exists), breaking ties by ascending NodeID for full determinism. This is
// deliberately a single-signal preference, not a weighted or multi-factor
// score — v1.md §19 excludes an "AI scheduler" from scope.
//
// Known, accepted characteristic: because this is deterministic,
// back-to-back similar submissions can keep picking the same node until
// its heartbeat catches up, colliding with the one-workload-per-node rule
// (rejected, reported FAILED — v1's existing, tested behavior). v1's
// arbitrary map order accidentally spread load; this trades that for
// explainability. Not addressed here — would mean consulting
// WorkloadRegistry's in-flight state from placement, a reasonable v2.1
// follow-up.
func selectNode(candidates []*NodeRecord, capability domain.CapabilityName, req domain.ResourceRequirements) (*NodeRecord, error) {
	var best *NodeRecord
	var reasons []string

	for _, rec := range candidates {
		ok, reason := nodeFits(rec, capability, req)
		if !ok {
			reasons = append(reasons, fmt.Sprintf("%s: %s", rec.Node.Identity.NodeID, reason))
			continue
		}
		switch {
		case best == nil:
			best = rec
		case rec.LastMetrics.MemoryAvailableBytes > best.LastMetrics.MemoryAvailableBytes:
			best = rec
		case rec.LastMetrics.MemoryAvailableBytes == best.LastMetrics.MemoryAvailableBytes &&
			rec.Node.Identity.NodeID < best.Node.Identity.NodeID:
			best = rec
		}
	}

	if best == nil {
		if len(candidates) == 0 {
			return nil, ErrNoReadyNode
		}
		return nil, fmt.Errorf("%w (%v)", ErrNoEligibleNode, reasons)
	}
	return best, nil
}
