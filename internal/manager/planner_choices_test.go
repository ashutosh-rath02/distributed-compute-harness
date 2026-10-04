package manager

import (
	"slices"
	"testing"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Choices that aren't models (a whisper.cpp model file, a doc.text method)
// reach the planner exactly as devices list them: only Ollama models have
// a normal form (":latest"), and "ocr:latest" would be no method at all.
func TestPlannerOffersNonModelChoicesAsListed(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.Registry.Upsert(domain.Manifest{Node: domain.Node{Identity: domain.Identity{NodeID: "pc"}, Name: "pc"}, Capabilities: []domain.Capability{
		{Name: "audio.transcribe", Version: "1", Attributes: map[string]string{catalog.AttrWhisperModels: "base.en,small"}},
		{Name: "doc.text", Version: "1", Attributes: map[string]string{catalog.AttrDocMethods: "ocr,text"}},
		{Name: "llm.generate", Version: "1", Attributes: map[string]string{catalog.AttrModels: "Gemma3"}},
	}}, nil)
	s.Registry.SetState("pc", domain.NodeReady)
	got := map[domain.CapabilityName]map[string][]string{}
	for _, p := range s.plannableTypes() {
		got[p.Name] = p.choices
	}
	if c := got["audio.transcribe"]["model"]; !slices.Equal(c, []string{"base.en", "small"}) {
		t.Errorf("whisper models %q", c)
	}
	if c := got["doc.text"]["method"]; !slices.Equal(c, []string{"ocr", "text"}) {
		t.Errorf("doc.text methods %q", c)
	}
	if c := got["llm.generate"]["model"]; !slices.Equal(c, []string{"gemma3:latest"}) {
		t.Errorf("ollama models %q", c)
	}
}
