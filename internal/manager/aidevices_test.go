package manager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"home-harness/internal/domain"
)

// The AI models card lists every model a device has — embedding models
// too, which llm.generate no longer offers — from llm.remove, and from
// llm.generate for an agent from before that.
func TestAIDevicesListEveryModelTheDeviceHas(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	caps := func(remove bool) []domain.Capability {
		c := []domain.Capability{
			{Name: "llm.pull", Version: "1"},
			{Name: "llm.generate", Version: "1", Attributes: map[string]string{"models": "gemma3:4b", "modelSizes": "gemma3:4b=3"}},
		}
		if remove {
			c = append(c, domain.Capability{Name: "llm.remove", Version: "1", Attributes: map[string]string{
				"models": "embeddinggemma:latest,gemma3:4b", "modelSizes": "embeddinggemma:latest=1,gemma3:4b=3"}})
		}
		return c
	}
	s.Registry.Upsert(domain.Manifest{Node: domain.Node{Identity: domain.Identity{NodeID: "new"}, Name: "new"}, Capabilities: caps(true)}, nil)
	s.Registry.Upsert(domain.Manifest{Node: domain.Node{Identity: domain.Identity{NodeID: "old"}, Name: "old"}, Capabilities: caps(false)}, nil)
	rec := httptest.NewRecorder()
	s.apiListAIDevices(rec, httptest.NewRequest(http.MethodGet, "/ai-devices", nil))
	var out []aiDevice
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("%v %s", err, rec.Body)
	}
	got := map[domain.NodeID][]aiModel{}
	for _, d := range out {
		got[d.NodeID] = d.Models
	}
	if m := got["new"]; len(m) != 2 || m[0] != (aiModel{"embeddinggemma:latest", 1}) || m[1] != (aiModel{"gemma3:4b", 3}) {
		t.Fatalf("new agent: %+v", m)
	}
	if m := got["old"]; len(m) != 1 || m[0] != (aiModel{"gemma3:4b", 3}) {
		t.Fatalf("old agent: %+v", m)
	}
}

// GET /models lists every model on a ready device, marking those that
// write text: harnessctl ask must not default to an embedding model.
func TestModelsListMarksEmbeddingModels(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.Registry.Upsert(domain.Manifest{Node: domain.Node{Identity: domain.Identity{NodeID: "new"}}, Capabilities: []domain.Capability{
		{Name: "llm.generate", Version: "1", Attributes: map[string]string{"models": "gemma3:4b"}},
		{Name: "llm.chat", Version: "1", Attributes: map[string]string{"models": "gemma3:4b"}},
		{Name: "llm.remove", Version: "1", Attributes: map[string]string{"models": "embeddinggemma:latest,gemma3:4b"}},
	}}, nil)
	s.Registry.Upsert(domain.Manifest{Node: domain.Node{Identity: domain.Identity{NodeID: "old"}}, Capabilities: []domain.Capability{
		{Name: "llm.generate", Version: "1", Attributes: map[string]string{"models": "llama3.2:latest"}},
	}}, nil)
	s.Registry.SetState("new", domain.NodeReady)
	s.Registry.SetState("old", domain.NodeReady)
	rec := httptest.NewRecorder()
	s.apiListModels(rec, httptest.NewRequest(http.MethodGet, "/models", nil))
	var rows []struct {
		Model string
		Nodes []struct{ ID string }
		Text  bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 3 {
		t.Fatalf("%v %s", err, rec.Body)
	}
	want := map[string]bool{"embeddinggemma:latest": false, "gemma3:4b": true, "llama3.2:latest": true}
	for _, r := range rows {
		if text, ok := want[r.Model]; !ok || r.Text != text || len(r.Nodes) != 1 {
			t.Errorf("row %+v", r)
		}
	}
}
