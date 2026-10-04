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

// couldEverFit reports whether rec could run a workload with req once it
// is idle: the capability, declared cores, and declared total memory. Live
// figures (free memory, CPU load) are deliberately left out — they reflect
// what is running right now, so a busy node failing them means "queue",
// not "reject". A node that declares no total memory falls back to its
// live free memory, as before.
func couldEverFit(rec *NodeRecord, capability domain.CapabilityName, req domain.ResourceRequirements, features ...string) (bool, string) {
	if !rec.HasCapability(capability) {
		return false, fmt.Sprintf("does not declare capability %q", capability)
	}
	if !offersVersion(rec, capability) {
		return false, fmt.Sprintf("offers a different version of %q (update the agent)", capability)
	}
	for _, f := range features {
		if !rec.hasAgentFeature(f) {
			return false, fmt.Sprintf("agent too old: lacks %q (update it)", f)
		}
	}
	if req.MinCPUCores > 0 {
		cores, ok := domain.StaticCapacity(rec.Resources, domain.ResourceCPUCores)
		if !ok || cores < req.MinCPUCores {
			return false, fmt.Sprintf("requires %.2f CPU cores, node declares %.2f", req.MinCPUCores, cores)
		}
	}
	if req.MinMemoryBytes > 0 {
		total, declared := domain.StaticCapacity(rec.Resources, domain.ResourceMemoryBytes)
		switch {
		case declared && total < float64(req.MinMemoryBytes):
			return false, fmt.Sprintf("requires %d bytes of memory, node has %.0f in total", req.MinMemoryBytes, total)
		case !declared && !rec.LastMetrics.LastHeartbeat.IsZero() && rec.LastMetrics.MemoryAvailableBytes < req.MinMemoryBytes:
			return false, fmt.Sprintf("requires %d bytes available memory, node has %d", req.MinMemoryBytes, rec.LastMetrics.MemoryAvailableBytes)
		}
	}
	return true, ""
}

// selectNode picks the best candidate satisfying req: the one with the
// most available memory (the only live, absolute resource signal that
// exists), breaking ties by ascending NodeID for full determinism. This is
// deliberately a single-signal preference, not a weighted or multi-factor
// score — v1.md §19 excludes an "AI scheduler" from scope.
//
// selectNode is selectNodeWithUsage for an idle fleet (no reservations).
// Real placement (resolveWorkloadTarget) uses selectNodeWithUsage with what
// is already reserved on each node (queue.go).
func selectNode(candidates []*NodeRecord, capability domain.CapabilityName, req domain.ResourceRequirements) (*NodeRecord, error) {
	return selectNodeWithUsage(candidates, nil, capability, req)
}

// selectNodeWithUsage is selectNode that, given current reservations,
// prefers the least busy node (the smallest share of its slots in use),
// then the one with the most memory left after them — spreading a burst
// of submissions (a job's tasks) over the fleet instead of piling onto
// whichever device has the most memory to spare while it still has free
// slots. Whether work fits at all is hasRoom's and nodeFits' call; this
// only ranks the nodes it fits on.
func selectNodeWithUsage(candidates []*NodeRecord, usage map[domain.NodeID]nodeUsage, capability domain.CapabilityName, req domain.ResourceRequirements) (*NodeRecord, error) {
	return selectNodeLocal(candidates, usage, nil, capability, req)
}

// selectNodeLocal is selectNodeWithUsage that, between equally busy nodes,
// prefers the one already holding more bytes of the workload's input files
// (local, by node: locality.go). The full order:
//
//  1. least busy: the smallest share of its slots in use;
//  2. most bytes of the workload's inputs already on the node;
//  3. most memory left after reservations;
//  4. lowest NodeID.
//
// Spreading stays first on purpose: a job's tasks on one big file still go
// to every device with a free slot, and the file's device only wins among
// equals (on an idle fleet: the first task, or a second job on the file).
func selectNodeLocal(candidates []*NodeRecord, usage map[domain.NodeID]nodeUsage, local map[domain.NodeID]int64, capability domain.CapabilityName, req domain.ResourceRequirements) (*NodeRecord, error) {
	var best *NodeRecord
	var reasons []string

	for _, rec := range candidates {
		ok, reason := nodeFits(rec, capability, req)
		if !ok {
			reasons = append(reasons, fmt.Sprintf("%s: %s", rec.Node.Identity.NodeID, reason))
			continue
		}
		if best == nil {
			best = rec
			continue
		}
		ru, bu := usage[rec.Node.Identity.NodeID], usage[best.Node.Identity.NodeID]
		free := func(r *NodeRecord, u nodeUsage) uint64 {
			if u.memory >= r.LastMetrics.MemoryAvailableBytes {
				return 0
			}
			return r.LastMetrics.MemoryAvailableBytes - u.memory
		}
		rf, bf := free(rec, ru), free(best, bu)
		// running/slots compared without division.
		rl, bl := ru.running*best.slots(), bu.running*rec.slots()
		rh, bh := local[rec.Node.Identity.NodeID], local[best.Node.Identity.NodeID]
		switch {
		case rl != bl:
			if rl < bl {
				best = rec
			}
		case rh != bh:
			if rh > bh {
				best = rec
			}
		case rf != bf:
			if rf > bf {
				best = rec
			}
		case rec.Node.Identity.NodeID < best.Node.Identity.NodeID:
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
