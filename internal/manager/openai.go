package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// The OpenAI-compatible chat API: any app that speaks OpenAI's chat API
// (chat UIs, editor plugins, scripts using the openai library) can use the
// fleet's local models by pointing its base URL at the manager:
//
//	GET  /v1/models              the models READY devices can chat with
//	POST /v1/chat/completions    one chat turn; "stream": true for SSE chunks
//
// Each request becomes an ordinary llm.chat workload — placed on a device
// that has the model, under the policy, queued when every such device is
// busy — and its output, streamed by the agent, is relayed as
// chat.completion chunks. The request's messages are kept with the
// workload like any task's parameters (GET /workloads/{id}); the audit
// log records only that a chat ran.
//
// Credentials: the operator token, the dashboard session, or the AI key
// (aiKeyName in the manager's state). The AI key works only here, so an
// app given it can chat with the models but can't run tasks, add devices
// or read anything else. The API listens where the operator API does —
// loopback by default.
//
// Not supported: tools/function calling, images and audio, n > 1,
// logprobs. Requests that need them are refused, never answered as if
// they had been honoured.

const (
	capLLMChat = domain.CapabilityName("llm.chat")
	// maxChatRequest bounds a request body; the conversation itself is
	// bounded by llm.chat's messages parameter (64 KiB once encoded).
	maxChatRequest = 1 << 20
	// chatPoll re-reads a chat's record between workload events, which the
	// event bus may drop when a subscriber is slow.
	chatPoll = 500 * time.Millisecond
)

// isOpenAIPath reports whether path is part of the OpenAI-compatible API.
func isOpenAIPath(path string) bool { return strings.HasPrefix(path, "/v1/") }

func writeOpenAIError(w http.ResponseWriter, status int, typ, code, msg string) {
	body := map[string]any{"message": msg, "type": typ, "param": nil, "code": nil}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": body})
}

// chatModels lists, per normalized model name, the READY devices that
// offer llm.chat with it.
func (s *Server) chatModels() map[string][]domain.NodeID {
	out := map[string][]domain.NodeID{}
	for _, rec := range s.Registry.List() {
		if rec.State != domain.NodeReady || !offersVersion(rec, capLLMChat) {
			continue
		}
		for _, m := range attrValues(rec, capLLMChat, catalog.AttrModels) {
			m = catalog.NormalizeModel(m)
			out[m] = append(out[m], rec.Node.Identity.NodeID)
		}
	}
	return out
}

func (s *Server) apiOpenAIModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := []model{}
	for m := range s.chatModels() {
		data = append(data, model{ID: m, Object: "model", OwnedBy: "home-harness"})
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// apiGetAIInfo is GET /ai-info (operator credentials only): what an app
// needs to use the fleet's models — the base URL, the AI key, and the
// models it can ask for now.
func (s *Server) apiGetAIInfo(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	models := []string{}
	for m := range s.chatModels() {
		models = append(models, m)
	}
	sort.Strings(models)
	writeJSON(w, http.StatusOK, map[string]any{"baseUrl": "http://" + host + "/v1", "apiKey": s.cfg.AIKey, "models": models})
}

// openAIChatRequest is the part of OpenAI's chat request this API reads;
// other fields are ignored, as OpenAI-compatible servers commonly do.
type openAIChatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Temperature         *float64        `json:"temperature"`
	MaxTokens           *int64          `json:"max_tokens"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens"`
	Seed                *int64          `json:"seed"`
	N                   *int            `json:"n"`
	Tools               json.RawMessage `json:"tools"`
	Functions           json.RawMessage `json:"functions"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// messageText reads a message's content: a string, or a list of parts of
// which only text parts are supported.
func messageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("content must be a string or a list of text parts")
	}
	var texts []string
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content of type %q isn't supported here: only text", p.Type)
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n"), nil
}

// nonEmptyJSON reports whether raw holds something other than null or an
// empty list.
func nonEmptyJSON(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != "[]"
}

// chatParams turns a request into llm.chat parameters, or says why it
// can't be served.
func chatParams(req openAIChatRequest) (map[string]string, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("model is required")
	}
	if req.N != nil && *req.N != 1 {
		return nil, errors.New("n must be 1: one answer per request")
	}
	if nonEmptyJSON(req.Tools) || nonEmptyJSON(req.Functions) {
		return nil, errors.New("tools and function calling aren't supported")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("messages is empty")
	}
	msgs := make([]chatMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		role := m.Role
		switch role {
		case "system", "user", "assistant":
		case "developer": // OpenAI's newer name for system instructions
			role = "system"
		default:
			return nil, fmt.Errorf("messages[%d]: role %q isn't supported (system, user, assistant)", i, m.Role)
		}
		text, err := messageText(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %v", i, err)
		}
		msgs = append(msgs, chatMessage{role, text})
	}
	encoded, _ := json.Marshal(msgs)
	if limit := 64 << 10; len(encoded) > limit {
		return nil, fmt.Errorf("the conversation is %d bytes; at most %d fit in one request", len(encoded), limit)
	}
	params := map[string]string{"model": catalog.NormalizeModel(req.Model), "messages": string(encoded)}
	if req.Temperature != nil {
		params["temperature"] = strconv.FormatFloat(*req.Temperature, 'g', -1, 64)
	}
	max := req.MaxCompletionTokens
	if max == nil {
		max = req.MaxTokens
	}
	if max != nil {
		params["max_tokens"] = strconv.FormatInt(*max, 10)
	}
	if req.Seed != nil {
		params["seed"] = strconv.FormatInt(*req.Seed, 10)
	}
	return params, nil
}

func (s *Server) apiOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var req openAIChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxChatRequest)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "", "the request isn't valid JSON: "+err.Error())
		return
	}
	params, err := chatParams(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "", err.Error())
		return
	}
	model := params["model"]
	models := s.chatModels()
	if _, ok := models[model]; !ok {
		names := make([]string, 0, len(models))
		for m := range models {
			names = append(names, m)
		}
		sort.Strings(names)
		have := "no device is offering any model right now"
		if len(names) > 0 {
			have = "models on the ready devices: " + strings.Join(names, ", ")
		}
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("no ready device has the model %q (%s)", req.Model, have))
		return
	}

	// Every device with the model holding work back (on battery, in use,
	// paused by the operator): say so now instead of leaving the app
	// waiting in the queue. Merely busy devices still queue it.
	var held []string
	available := false
	for _, id := range models[model] {
		if rec, ok := s.Registry.Get(id); ok {
			ok, reason := s.availableNow(rec, time.Now())
			if ok {
				available = true
				break
			}
			held = append(held, s.nodeDisplayName(id)+": "+reason)
		}
	}
	if !available {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "device_unavailable",
			fmt.Sprintf("no device with %q is taking work right now (%s)", model, strings.Join(held, "; ")))
		return
	}

	// Subscribed before submitting, so no event about the new workload
	// can slip by unseen.
	events, unsubscribe := s.Events.Subscribe(64)
	defer unsubscribe()
	wl, err := s.Submit(r.Context(), WorkloadSpec{Capability: capLLMChat, Params: params})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidWorkload):
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "", err.Error())
		case errors.Is(err, ErrPolicy):
			writeOpenAIError(w, http.StatusForbidden, "permission_error", "", err.Error())
		case errors.Is(err, ErrNodeNotConnected), errors.Is(err, ErrNoReadyNode), errors.Is(err, ErrNoEligibleNode):
			writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "", err.Error())
		default:
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "", err.Error())
		}
		return
	}
	// High-volume and app-driven: recorded in the noise log, never with the
	// conversation itself.
	s.audit(domain.AuditNoise, "ai.chat", "", actorFrom(r.Context()), map[string]any{"workloadId": string(wl.ID), "model": model, "stream": req.Stream})

	c := &chatRun{s: s, id: wl.ID, events: events, model: model, created: time.Now().Unix()}
	w.Header().Set("X-Home-Harness-Workload", string(wl.ID))
	if req.Stream {
		c.stream(w, r, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		return
	}
	var text strings.Builder
	res, err := c.follow(r.Context(), func(delta string) error { text.WriteString(delta); return nil })
	if err != nil {
		c.abandon(r.Context())
		return
	}
	if res.err != "" {
		writeOpenAIError(w, http.StatusBadGateway, "server_error", "", res.err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": c.completionID(), "object": "chat.completion", "created": c.created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": res.finish, "logprobs": nil,
		}},
		"usage": res.usage(),
	})
}

// chatRun follows one chat's workload.
type chatRun struct {
	s       *Server
	id      domain.WorkloadID
	events  <-chan domain.Event
	model   string
	created int64
}

// chatResult is how a chat ended: err is set when it produced no proper
// answer (failed, canceled, device lost).
type chatResult struct {
	finish           string
	err              string
	prompt, complete int
}

func (r chatResult) usage() map[string]int {
	return map[string]int{"prompt_tokens": r.prompt, "completion_tokens": r.complete, "total_tokens": r.prompt + r.complete}
}

func (c *chatRun) completionID() string { return "chatcmpl-" + string(c.id) }

// abandon cancels the workload of a chat whose client went away, queued
// or running.
func (c *chatRun) abandon(ctx context.Context) {
	if rec, ok := c.s.Workloads.Get(c.id); ok && !chatOver(rec.Status.State) {
		c.s.CancelWorkload(context.WithoutCancel(ctx), c.id)
	}
}

// chatOver reports whether a chat's workload has ended (UNKNOWN: the
// manager restarted while it ran, and its outcome is lost).
func chatOver(state domain.WorkloadState) bool {
	switch state {
	case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled, domain.WorkloadUnknown:
		return true
	}
	return false
}

// chatUsageLine is the last stderr line of an llm.chat run (tasks'
// ollamaChat): "<model>: P prompt tokens, C tokens[, R tokens/s] (reason)".
var chatUsageLine = regexp.MustCompile(`(\d+) prompt tokens, (\d+) tokens(?:, [0-9.]+ tokens/s)? \(([a-z_]*)\)`)

// follow relays the chat's text to emit as it arrives and returns how it
// ended. It returns an error only when ctx ends first (the client left).
func (c *chatRun) follow(ctx context.Context, emit func(string) error) (chatResult, error) {
	sent := 0
	tick := time.NewTicker(chatPoll)
	defer tick.Stop()
	for {
		rec, ok := c.s.Workloads.Get(c.id)
		if !ok {
			return chatResult{err: "the chat's task disappeared from the manager"}, nil
		}
		st := rec.Status
		done := chatOver(st.State)
		if out := st.Stdout; len(out) > sent {
			delta := out[sent:]
			if !done {
				delta = completeRunes(delta)
			}
			if delta != "" {
				if err := emit(delta); err != nil {
					return chatResult{}, err
				}
				sent += len(delta)
			}
		}
		if done {
			return c.finish(st, sent, emit)
		}
		select {
		case <-ctx.Done():
			return chatResult{}, ctx.Err()
		case <-c.events:
		case <-tick.C:
		}
	}
}

// finish settles a terminal chat: the rest of a long answer from its
// output file (the live text stops at the output cap), usage and the
// finish reason from the agent's last line.
func (c *chatRun) finish(st domain.WorkloadStatus, sent int, emit func(string) error) (chatResult, error) {
	switch st.State {
	case domain.WorkloadCompleted:
	case domain.WorkloadCanceled:
		return chatResult{err: "the chat was canceled before it finished"}, nil
	default:
		msg := st.Error
		if st.NodeLost {
			msg = "the device answering went away: " + msg
		}
		if msg == "" {
			msg = "the chat failed (" + string(st.State) + ")"
		}
		return chatResult{err: msg}, nil
	}
	res := chatResult{finish: "stop"}
	if st.Truncated {
		res.finish = "length"
		if rest, ok := c.restOfAnswer(st, sent); ok {
			if rest != "" {
				if err := emit(rest); err != nil {
					return chatResult{}, err
				}
			}
			res.finish = "stop"
		}
	}
	if m := chatUsageLine.FindAllStringSubmatch(st.Stderr, -1); len(m) > 0 {
		last := m[len(m)-1]
		res.prompt, _ = strconv.Atoi(last[1])
		res.complete, _ = strconv.Atoi(last[2])
		if last[3] == "length" {
			res.finish = "length"
		}
	}
	return res, nil
}

// restOfAnswer reads the answer past the first sent bytes from the chat's
// response.txt output.
func (c *chatRun) restOfAnswer(st domain.WorkloadStatus, sent int) (string, bool) {
	if c.s.cfg.Artifacts == nil {
		return "", false
	}
	for _, o := range st.Outputs {
		if o.Name != "response.txt" {
			continue
		}
		f, _, err := c.s.cfg.Artifacts.Open(o.SHA256)
		if err != nil {
			return "", false
		}
		defer f.Close()
		all, err := io.ReadAll(io.LimitReader(f, c.s.cfg.Artifacts.MaxBytes()))
		if err != nil || len(all) < sent {
			return "", false
		}
		return string(all[sent:]), true
	}
	return "", false
}

// completeRunes drops a trailing partial UTF-8 sequence, which arrives
// with the next update.
func completeRunes(s string) string {
	for i := 0; i < utf8.UTFMax && len(s) > 0 && !utf8.ValidString(s); i++ {
		s = s[:len(s)-1]
	}
	return s
}

// stream answers as Server-Sent Events: a role chunk, a chunk per new
// piece of text, a closing chunk with the finish reason (and, when asked,
// one with usage), then [DONE]. A failure after the stream started is
// sent as an error event, which OpenAI's client libraries raise.
func (c *chatRun) stream(w http.ResponseWriter, r *http.Request, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.abandon(r.Context())
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(v any) error {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": c.completionID(), "object": "chat.completion.chunk", "created": c.created, "model": c.model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}},
		}
	}
	if send(chunk(map[string]any{"role": "assistant", "content": ""}, nil)) != nil {
		c.abandon(r.Context())
		return
	}
	res, err := c.follow(r.Context(), func(delta string) error {
		return send(chunk(map[string]any{"content": delta}, nil))
	})
	if err != nil {
		c.abandon(r.Context())
		return
	}
	if res.err != "" {
		send(map[string]any{"error": map[string]any{"message": res.err, "type": "server_error", "param": nil, "code": nil}})
		return
	}
	send(chunk(map[string]any{}, res.finish))
	if includeUsage {
		send(map[string]any{
			"id": c.completionID(), "object": "chat.completion.chunk", "created": c.created, "model": c.model,
			"choices": []any{}, "usage": res.usage(),
		})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
