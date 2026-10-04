package domain

// GPU is a graphics processor a device reports (Manifest.GPUs), for
// placing AI work where a model fits in fast memory.
type GPU struct {
	Name   string `json:"name"`
	Vendor string `json:"vendor,omitempty"` // nvidia, amd, intel, apple
	// MemoryBytes: dedicated video memory; for an integrated or unified
	// GPU, the share of system memory the driver reports (informational).
	MemoryBytes uint64 `json:"memoryBytes,omitempty"`
	// Integrated: shares system memory (laptop graphics, Apple silicon).
	// Its memory is never counted as room a model fits in on a GPU.
	Integrated bool `json:"integrated,omitempty"`
}

// DedicatedGPUMemory sums the dedicated video memory of gpus.
func DedicatedGPUMemory(gpus []GPU) uint64 {
	var total uint64
	for _, g := range gpus {
		if !g.Integrated {
			total += g.MemoryBytes
		}
	}
	return total
}
