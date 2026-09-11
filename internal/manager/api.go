package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"home-harness/internal/domain"
)

// nodeView is the JSON shape returned for a node — a flattened,
// API-stable projection of NodeRecord, not the internal type itself.
type nodeView struct {
	NodeID       domain.NodeID       `json:"nodeId"`
	Name         string              `json:"name"`
	Hostname     string              `json:"hostname"`
	Platform     domain.Platform     `json:"platform"`
	AgentVersion string              `json:"agentVersion"`
	State        domain.NodeState    `json:"state"`
	LastSeen     time.Time           `json:"lastSeen"`
	Metrics      domain.RuntimeState `json:"metrics"`
}

func toNodeView(rec *NodeRecord) nodeView {
	return nodeView{
		NodeID:       rec.Node.Identity.NodeID,
		Name:         rec.Node.Name,
		Hostname:     rec.Node.Hostname,
		Platform:     rec.Node.Platform,
		AgentVersion: rec.Node.AgentVersion,
		State:        rec.State,
		LastSeen:     rec.LastSeen,
		Metrics:      rec.LastMetrics,
	}
}

// NewHTTPHandler returns the manager's observability/control HTTP API
// (v1.md §15 "Basic API"):
//
//	GET  /nodes                    list all known nodes
//	GET  /nodes/{id}                one node's identity/state/metrics
//	GET  /nodes/{id}/resources       one node's declared resources
//	GET  /nodes/{id}/capabilities    one node's declared capabilities
//	GET  /resources/total           resource totals summed across all nodes
//	GET  /events                    Server-Sent Events stream of harness events
//	POST /nodes/{id}/commands        dispatch a command: {"name":"...","args":{...},"timeoutMs":...}
//
// It is a thin adapter over Registry/SendCommand/Events — the manager's
// core logic has no HTTP dependency of its own.
func (s *Server) NewHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nodes", s.apiListNodes)
	mux.HandleFunc("GET /nodes/{id}", s.apiGetNode)
	mux.HandleFunc("GET /nodes/{id}/resources", s.apiGetNodeResources)
	mux.HandleFunc("GET /nodes/{id}/capabilities", s.apiGetNodeCapabilities)
	mux.HandleFunc("GET /resources/total", s.apiGetTotalResources)
	mux.HandleFunc("GET /events", s.apiEvents)
	mux.HandleFunc("POST /nodes/{id}/commands", s.apiPostCommand)
	return mux
}

func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	recs := s.Registry.List()
	views := make([]nodeView, 0, len(recs))
	for _, rec := range recs {
		views = append(views, toNodeView(rec))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) apiGetNode(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Registry.Get(domain.NodeID(r.PathValue("id")))
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toNodeView(rec))
}

func (s *Server) apiGetNodeResources(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Registry.Get(domain.NodeID(r.PathValue("id")))
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec.Resources)
}

func (s *Server) apiGetNodeCapabilities(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Registry.Get(domain.NodeID(r.PathValue("id")))
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec.Capabilities)
}

func (s *Server) apiGetTotalResources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Registry.TotalResources())
}

type commandRequest struct {
	Name      domain.CommandName `json:"name"`
	Args      map[string]string  `json:"args"`
	TimeoutMS int                `json:"timeoutMs"`
}

const defaultCommandTimeout = 5 * time.Second

func (s *Server) apiPostCommand(w http.ResponseWriter, r *http.Request) {
	nodeID := domain.NodeID(r.PathValue("id"))

	var req commandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	timeout := defaultCommandTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	result, err := s.SendCommand(r.Context(), nodeID, req.Name, req.Args, timeout)
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeNotConnected):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusGatewayTimeout)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsubscribe := s.Events.Subscribe(16)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
