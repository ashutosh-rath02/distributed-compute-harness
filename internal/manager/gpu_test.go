package manager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func gpuNode(id domain.NodeID, gpus []domain.GPU, sizes string) *NodeRecord {
	return &NodeRecord{
		Node: domain.Node{Identity: domain.Identity{NodeID: id}},
		GPUs: gpus,
		Capabilities: []domain.Capability{{Name: capLLMChat, Version: "1", Attributes: map[string]string{
			"models": "llama3.2:latest,qwen2.5:14b", "modelSizes": sizes,
		}}},
	}
}

func ids(recs []*NodeRecord) string {
	var out []string
	for _, r := range recs {
		out = append(out, string(r.Node.Identity.NodeID))
	}
	return strings.Join(out, ",")
}

func TestPreferGPUFitIsOnlyAPreference(t *testing.T) {
	sizes := "llama3.2:latest=2019393189,qwen2.5:14b=9019393189"
	gaming := gpuNode("gaming-pc", []domain.GPU{{Name: "RTX 3060", Vendor: "nvidia", MemoryBytes: 8 << 30}}, sizes)
	laptop := gpuNode("laptop", []domain.GPU{{Name: "Iris Xe", Vendor: "intel", MemoryBytes: 16 << 30, Integrated: true}}, sizes)
	plain := gpuNode("plain", nil, sizes)
	all := []*NodeRecord{laptop, gaming, plain}
	chat := func(model string) placement {
		return placement{capability: capLLMChat, params: map[string]string{"model": model, "messages": "[]"}}
	}
	for name, c := range map[string]struct {
		p    placement
		want string
	}{
		"fits the dedicated GPU":           {chat("llama3.2"), "gaming-pc"},
		"too big for any GPU: all of them": {chat("qwen2.5:14b"), "laptop,gaming-pc,plain"},
		"not an AI task: all of them":      {placement{capability: "system.identity"}, "laptop,gaming-pc,plain"},
		"model size not reported: all":     {chat("mistral"), "laptop,gaming-pc,plain"},
		"integrated memory never counts":   {chat("llama3.2"), "gaming-pc"},
	} {
		if got := ids(preferGPUFit(all, c.p)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	// Never narrows to nothing, and a single candidate stays.
	if got := ids(preferGPUFit([]*NodeRecord{laptop}, chat("llama3.2"))); got != "laptop" {
		t.Errorf("one candidate: %s", got)
	}
}

// Through real placement: the device whose GPU fits the model wins over
// one with more free memory, which would win without GPUs.
func TestPlacementPrefersTheGPUThatFitsOverFreeMemory(t *testing.T) {
	r := NewRegistry()
	s := &Server{Registry: r, Workloads: NewWorkloadRegistry()}
	add := func(id domain.NodeID, free uint64, gpus []domain.GPU) {
		m := testNode(id)
		m.GPUs = gpus
		r.Upsert(m, &fakeConn{tag: string(id)})
		r.SetState(id, domain.NodeReady)
		r.UpdateResources(id, nil, []domain.Capability{{Name: "llm.generate", Version: "1", Attributes: map[string]string{
			"models": "llama3.2:latest", "modelSizes": "llama3.2:latest=2019393189",
		}}})
		r.RecordHeartbeat(id, domain.RuntimeState{MemoryAvailableBytes: free, LastHeartbeat: time.Now()})
	}
	add("big-ram", 60<<30, nil)
	add("gpu-box", 8<<30, []domain.GPU{{Name: "RTX 3060", Vendor: "nvidia", MemoryBytes: 12 << 30}})
	p := placement{capability: "llm.generate", params: map[string]string{"model": "llama3.2", "prompt": "hi"}}
	if _, id, err := s.resolve(p, nil); err != nil || id != "gpu-box" {
		t.Fatalf("an AI task: %s %v, want gpu-box", id, err)
	}
	r.UpdateResources("gpu-box", nil, []domain.Capability{{Name: "llm.generate", Version: "1", Attributes: map[string]string{
		"models": "llama3.2:latest", "modelSizes": "llama3.2:latest=20193931890", // 20 GB: fits no GPU here
	}}})
	r.UpdateResources("big-ram", nil, []domain.Capability{{Name: "llm.generate", Version: "1", Attributes: map[string]string{
		"models": "llama3.2:latest", "modelSizes": "llama3.2:latest=20193931890",
	}}})
	if _, id, err := s.resolve(p, nil); err != nil || id != "big-ram" {
		t.Fatalf("a model too big for the GPU: %s %v, want big-ram (most free memory)", id, err)
	}
}

func TestModelTasksMustNameTheirDevice(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	for _, capability := range []domain.CapabilityName{"llm.pull", "llm.remove"} {
		_, err := s.Submit(context.Background(), WorkloadSpec{Capability: capability, Params: map[string]string{"model": "llama3.2"}})
		if !errors.Is(err, ErrInvalidWorkload) || !strings.Contains(err.Error(), "say which device") {
			t.Errorf("%s without a device: %v", capability, err)
		}
	}
}
