package manager

import (
	"bytes"
	"encoding/json"
	"fmt"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// A job's reduce gets each task's outputs at parts/<task>/<name>: the
// path numbers a result but doesn't say which file it came from (every
// llm.generate run writes response.txt). A reduce type that reports per
// file (catalog.Type.PartNames, report.collect) therefore also gets
// catalog.TaskIndexName: each task's key with the name it was given —
// the file's, for a job run once per file (harnessctl, the dashboard and
// the planner all name tasks so). Stored like any input; identical for
// every attempt of the reduce, so a retry reuses it.

// namesParts reports whether capability is a reduce that gets the index.
func namesParts(capability domain.CapabilityName) bool {
	t, ok := catalog.Lookup(capability)
	return ok && t.PartNames
}

// taskName is what the index calls a task: its name, else its last input
// (a task's own file comes after any shared ones).
func taskName(t domain.TaskSpec) string {
	if t.Name == "" && len(t.Inputs) > 0 {
		return t.Inputs[len(t.Inputs)-1].Name
	}
	return t.Name
}

// storeTaskIndex stores job's task index and returns it as an input.
func (s *Server) storeTaskIndex(job domain.Job) (domain.ArtifactRef, error) {
	if s.cfg.Artifacts == nil {
		return domain.ArtifactRef{}, fmt.Errorf("storing %s: this manager has no artifact store", catalog.TaskIndexName)
	}
	type entry struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	index := struct {
		Tasks []entry `json:"tasks"`
	}{Tasks: make([]entry, 0, len(job.Tasks))}
	for i, t := range job.Tasks {
		index.Tasks = append(index.Tasks, entry{domain.TaskKey(i), taskName(t)})
	}
	data, err := json.Marshal(index)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	info, err := s.cfg.Artifacts.Put(bytes.NewReader(data), "")
	if err != nil {
		return domain.ArtifactRef{}, fmt.Errorf("storing %s: %w", catalog.TaskIndexName, err)
	}
	return domain.ArtifactRef{Name: catalog.TaskIndexName, SHA256: info.SHA256, Size: info.Size}, nil
}
