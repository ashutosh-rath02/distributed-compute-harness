package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
)

// startManagerWithAPI starts a manager over WS on addr and returns both
// the server and an httptest.Server exposing its HTTP API, so tests can
// hit real HTTP endpoints rather than calling handlers directly.
func startManagerWithAPI(t *testing.T, addr string, heartbeatTimeout time.Duration) (*manager.Server, *httptest.Server) {
	t.Helper()
	srv := startManager(t, addr, heartbeatTimeout)
	apiSrv := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(apiSrv.Close)
	return srv, apiSrv
}

func TestAPIListAndGetNode(t *testing.T) {
	const addr = "127.0.0.1:19220"
	srv, apiSrv := startManagerWithAPI(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "api-agent-a")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	resp, err := http.Get(apiSrv.URL + "/nodes")
	if err != nil {
		t.Fatalf("GET /nodes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var nodes []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	resp2, err := http.Get(fmt.Sprintf("%s/nodes/%s", apiSrv.URL, a.NodeID()))
	if err != nil {
		t.Fatalf("GET /nodes/{id}: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	resp3, err := http.Get(apiSrv.URL + "/nodes/node-does-not-exist")
	if err != nil {
		t.Fatalf("GET /nodes/{unknown}: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown node, got %d", resp3.StatusCode)
	}
}

func TestAPIGetNodeResourcesAndTotals(t *testing.T) {
	const addr = "127.0.0.1:19221"
	srv, apiSrv := startManagerWithAPI(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "api-agent-b")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && len(rec.Resources) > 0
	})

	resp, err := http.Get(fmt.Sprintf("%s/nodes/%s/resources", apiSrv.URL, a.NodeID()))
	if err != nil {
		t.Fatalf("GET resources: %v", err)
	}
	defer resp.Body.Close()
	var resources []domain.Resource
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resources) == 0 {
		t.Fatal("expected at least one resource")
	}

	resp2, err := http.Get(apiSrv.URL + "/resources/total")
	if err != nil {
		t.Fatalf("GET /resources/total: %v", err)
	}
	defer resp2.Body.Close()
	var totals map[string]float64
	if err := json.NewDecoder(resp2.Body).Decode(&totals); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if totals[string(domain.ResourceCPUCores)] <= 0 {
		t.Fatalf("expected positive total CPU cores, got %v", totals)
	}
}

func TestAPIDispatchCommand(t *testing.T) {
	const addr = "127.0.0.1:19222"
	srv, apiSrv := startManagerWithAPI(t, addr, 2*time.Second)
	a := startRegisteredAgent(t, addr, "api-agent-c")

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})

	body, _ := json.Marshal(map[string]any{"name": "ECHO", "args": map[string]string{"message": "via-api"}})
	resp, err := http.Post(fmt.Sprintf("%s/nodes/%s/commands", apiSrv.URL, a.NodeID()), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST command: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result domain.CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Success || result.Output["message"] != "via-api" {
		t.Fatalf("expected echoed message via API, got %+v", result)
	}

	// Command to an unconnected node should fail with a client error, not
	// hang or 500.
	body2, _ := json.Marshal(map[string]any{"name": "PING"})
	resp2, err := http.Post(fmt.Sprintf("%s/nodes/%s/commands", apiSrv.URL, "node-unknown"), "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("POST command (unknown node): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for unconnected node, got %d", resp2.StatusCode)
	}
}

func TestAPIEventsStream(t *testing.T) {
	const addr = "127.0.0.1:19223"
	_, apiSrv := startManagerWithAPI(t, addr, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiSrv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	// Trigger an event by registering a node while we're listening.
	startRegisteredAgent(t, addr, "api-agent-events")

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(4 * time.Second)
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > 6 && line[:6] == "data: " {
			var evt domain.Event
			if err := json.Unmarshal([]byte(line[6:]), &evt); err == nil && evt.Type == domain.EventNodeRegistered {
				found = true
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if !found {
		t.Fatal("expected to observe a node.registered event on the SSE stream")
	}
}
