package manager

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// aiDevice is one device that runs local AI models (its Ollama answers):
// what it has, for the dashboard's "AI models" card.
type aiDevice struct {
	NodeID domain.NodeID    `json:"nodeId"`
	Name   string           `json:"name"`
	State  domain.NodeState `json:"state"`
	Models []aiModel        `json:"models"`
	GPUs   []domain.GPU     `json:"gpus,omitempty"`
	// DedicatedGPUBytes: the GPU memory a model can fit in (integrated
	// GPUs don't count).
	DedicatedGPUBytes uint64 `json:"dedicatedGpuBytes,omitempty"`
}

type aiModel struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

// apiListAIDevices is GET /ai-devices: every known device whose Ollama
// answers (it offers llm.pull), with its models and sizes and its GPUs.
func (s *Server) apiListAIDevices(w http.ResponseWriter, r *http.Request) {
	out := []aiDevice{}
	for _, rec := range s.Registry.List() {
		if !rec.HasCapability("llm.pull") {
			continue
		}
		id := rec.Node.Identity.NodeID
		d := aiDevice{NodeID: id, Name: s.nodeDisplayName(id), State: rec.State, Models: []aiModel{}, GPUs: rec.GPUs, DedicatedGPUBytes: domain.DedicatedGPUMemory(rec.GPUs)}
		sizes := map[string]int64{}
		for _, e := range attrValues(rec, "llm.generate", catalog.AttrModelSizes) {
			if name, size, ok := strings.Cut(e, "="); ok {
				sizes[name], _ = strconv.ParseInt(size, 10, 64)
			}
		}
		for _, m := range attrValues(rec, "llm.generate", catalog.AttrModels) {
			d.Models = append(d.Models, aiModel{Name: m, SizeBytes: sizes[m]})
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}
