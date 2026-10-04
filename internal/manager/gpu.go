package manager

import (
	"strconv"
	"strings"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// GPU awareness: among devices that could take an AI task right now,
// prefer those whose dedicated GPU memory holds the requested model (it
// runs many times faster there). Only a preference: it never rules a
// device out or holds work back — the model's file size is a lower bound
// on what it needs to run, and Ollama falls back to the CPU anyway.
// Integrated GPUs and Apple's unified memory don't count (domain.GPU).

// preferGPUFit narrows candidates to those whose dedicated GPU memory
// fits p's model, when p chooses a model and at least one does.
func preferGPUFit(candidates []*NodeRecord, p placement) []*NodeRecord {
	t, ok := catalog.Lookup(p.capability)
	if !ok || len(candidates) < 2 {
		return candidates
	}
	model := ""
	for _, param := range t.Params {
		if param.ChoicesAttr == catalog.AttrModels {
			model = catalog.NormalizeModel(p.params[param.Name])
		}
	}
	if model == "" {
		return candidates
	}
	var fit []*NodeRecord
	for _, rec := range candidates {
		size := modelSize(rec, p.capability, model)
		if vram := domain.DedicatedGPUMemory(rec.GPUs); size > 0 && vram >= uint64(size) {
			fit = append(fit, rec)
		}
	}
	if len(fit) == 0 {
		return candidates
	}
	return fit
}

// modelSize is model's size on rec, as its capability's model-sizes
// attribute reports it (0: not reported).
func modelSize(rec *NodeRecord, capability domain.CapabilityName, model string) int64 {
	for _, entry := range attrValues(rec, capability, catalog.AttrModelSizes) {
		name, size, ok := strings.Cut(entry, "=")
		if ok && catalog.NormalizeModel(name) == model {
			n, _ := strconv.ParseInt(size, 10, 64)
			return n
		}
	}
	return 0
}
