package manager

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"home-harness/internal/artifacts"
	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// The index names each task after its file: its name (what every client
// sets for a per-file job), else its own input, which comes after any
// shared ones.
func TestTaskIndexNamesEachTaskByItsFile(t *testing.T) {
	store, err := artifacts.Open(artifacts.Config{Dir: t.TempDir(), MaxBytes: 1 << 20, TotalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, nil, Config{Artifacts: store})
	sha := strings.Repeat("a", 64)
	job := domain.Job{Tasks: []domain.TaskSpec{
		{Name: "report q3.txt", Inputs: []domain.ArtifactRef{{Name: "report q3.txt", SHA256: sha}}},
		{Inputs: []domain.ArtifactRef{{Name: "prompt.txt", SHA256: sha}, {Name: "b.md", SHA256: sha}}},
		{},
	}}
	ref, err := s.storeTaskIndex(job)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != catalog.TaskIndexName || ref.Size == 0 {
		t.Fatalf("index input %+v", ref)
	}
	f, _, err := store.Open(ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != `{"tasks":[{"key":"0000","name":"report q3.txt"},{"key":"0001","name":"b.md"},{"key":"0002","name":""}]}` {
		t.Fatalf("index %s", data)
	}
	// Every attempt of the reduce gets the same file.
	if again, err := s.storeTaskIndex(job); err != nil || again != ref {
		t.Fatalf("again: %+v %v", again, err)
	}
	if namesParts("archive.zip") || !namesParts("report.collect") || namesParts("") {
		t.Fatal("only report.collect gets the index")
	}
	if _, err := NewServer(nil, nil, Config{}).storeTaskIndex(job); err == nil {
		t.Fatal("an index without an artifact store")
	}
}

// llm.embed isn't offered to the planner: its model choices (embedding
// models) would turn the plan schema's shared model parameter into "any
// value", and an embedding model could then be planned for a prompt.
func TestPlannerLeavesEmbeddingsOut(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.Registry.Upsert(domain.Manifest{Node: domain.Node{Identity: domain.Identity{NodeID: "pc"}}, Capabilities: []domain.Capability{
		{Name: "llm.generate", Version: "1", Attributes: map[string]string{"models": "gemma3:4b"}},
		{Name: "llm.classify", Version: "1", Attributes: map[string]string{"models": "gemma3:4b"}},
		{Name: "llm.embed", Version: "1", Attributes: map[string]string{"models": "embeddinggemma:latest"}},
		{Name: "report.collect", Version: "1"},
	}}, nil)
	s.Registry.SetState("pc", domain.NodeReady)
	types := s.plannableTypes()
	var names []string
	for _, ty := range types {
		names = append(names, string(ty.Name))
	}
	if got := strings.Join(names, ","); got != "llm.classify,llm.generate,report.collect" {
		t.Fatalf("plannable %s", got)
	}
	var schema struct {
		Properties struct {
			Task struct {
				Properties struct {
					Params struct {
						Properties map[string]json.RawMessage
					}
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(planSchema(types, nil)), &schema); err != nil {
		t.Fatal(err)
	}
	if got := string(schema.Properties.Task.Properties.Params.Properties["model"]); got != `{"type":"string","enum":["gemma3:4b"]}` {
		t.Fatalf("model in the plan schema: %s", got)
	}
}
