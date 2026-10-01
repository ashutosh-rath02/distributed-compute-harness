package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/tasks"
)

// A streaming task's progress reports must all come before its final
// status: one after it would make a finished workload look running.
func TestStreamingProgressNeverFollowsTheFinalStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m:latest"}}})
			return
		}
		for i := 0; i < 10; i++ {
			time.Sleep(15 * time.Millisecond)
			w.Write([]byte(`{"response":"x"}` + "\n"))
			w.(http.Flusher).Flush()
		}
		// A burst right at the end: the last output change lands just
		// before the final status, so a reporter still running then would
		// send it after.
		for i := 0; i < 5; i++ {
			w.Write([]byte(`{"response":"y"}` + "\n"))
		}
		w.Write([]byte(`{"done":true}` + "\n"))
	}))
	defer srv.Close()
	e := NewExecutor()
	e.SetHandlers(tasks.NewRegistry(tasks.Options{OllamaURL: srv.URL}))
	e.SetWorkRoot(t.TempDir())
	e.progressEvery = 30 * time.Millisecond
	for round := 0; round < 5; round++ {
		var mu sync.Mutex
		var states []domain.WorkloadState
		done := make(chan struct{})
		wl := domain.Workload{ID: domain.WorkloadID("0123456789abcdef0123456789abcde" + string(rune('0'+round))), Capability: "llm.generate",
			Params:  map[string]string{"model": "m", "prompt": "p", "temperature": "0.7", "max_tokens": "512", "seed": "0"},
			Outputs: []string{"response.txt"}}
		err := e.StartWithFiles(context.Background(), wl, &fakeTransfer{}, func(s domain.WorkloadStatus) {
			mu.Lock()
			states = append(states, s.State)
			mu.Unlock()
			if s.State != domain.WorkloadRunning {
				close(done)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		<-done
		time.Sleep(120 * time.Millisecond) // any straggling progress report would land now
		mu.Lock()
		if last := states[len(states)-1]; last != domain.WorkloadCompleted {
			t.Fatalf("round %d: statuses %v end with %s, want the final COMPLETED last", round, states, last)
		}
		if len(states) < 3 {
			t.Fatalf("round %d: expected progress reports while streaming, got %v", round, states)
		}
		mu.Unlock()
	}
}
