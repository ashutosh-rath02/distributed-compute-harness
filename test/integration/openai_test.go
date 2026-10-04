package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

const (
	testOperatorToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testAIKey         = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// startAIManager runs a manager with operator authentication and an AI
// key, as cmd/manager does.
func startAIManager(t *testing.T, addr string, p *domain.Policy) artifactManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second, ReconcileInterval: 100 * time.Millisecond,
		Artifacts: openArtifactStore(t, 1<<20), InitialPolicy: p, OperatorToken: testOperatorToken, AIKey: testAIKey,
	})
	transport.Handle("/workload-artifacts/", srv.ArtifactTransferHandler())
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	return artifactManager{srv: srv, api: api.URL}
}

// openAI sends an OpenAI-style request with the AI key, the way a chat
// app would.
func openAI(t *testing.T, ctx context.Context, method, url string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, r)
	req.Header.Set("Authorization", "Bearer "+testAIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func chatBody(model string, stream bool, user string) map[string]any {
	body := map[string]any{"model": model, "stream": stream, "messages": []map[string]string{
		{"role": "system", "content": "You are terse."}, {"role": "user", "content": user},
	}}
	if stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	return body
}

type sseChunk struct {
	Object  string `json:"object"`
	Choices []struct {
		Delta struct {
			Role    *string `json:"role"`
			Content *string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct{ Message string } `json:"error"`
}

// readSSE collects a chat stream's chunks; onContent sees each piece of
// text as it arrives. A chunk that isn't JSON ends it with an error.
func readSSE(body io.Reader, onContent func(string)) (chunks []sseChunk, done bool, err error) {
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			return chunks, true, nil
		}
		var c sseChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return chunks, false, fmt.Errorf("chunk %q: %w", data, err)
		}
		chunks = append(chunks, c)
		if len(c.Choices) > 0 && c.Choices[0].Delta.Content != nil && *c.Choices[0].Delta.Content != "" && onContent != nil {
			onContent(*c.Choices[0].Delta.Content)
		}
	}
	return chunks, false, nil
}

// A chat app's two calls, models and chat, against real agents: each
// request lands on the device with its model, answers both whole and
// streamed, in OpenAI's shapes.
func TestOpenAIChatRoutesByModelAndAnswersInOpenAIShapes(t *testing.T) {
	const addr = "127.0.0.1:19590"
	m := startAIManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	llama := &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"from ", "llama"}}
	qwen := &fakeOllama{models: []string{"qwen2.5:7b"}, chunks: []string{"from ", "qwen"}}
	a := startLLMAgent(t, ctx, addr, "llama-box", startFakeOllama(t, llama), 2)
	b := startLLMAgent(t, ctx, addr, "qwen-box", startFakeOllama(t, qwen), 2)
	for _, id := range []domain.NodeID{a.NodeID(), b.NodeID()} {
		waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	}

	resp := openAI(t, ctx, http.MethodGet, m.api+"/v1/models", nil)
	var models struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	if len(models.Data) != 2 || models.Data[0].ID != "llama3.2:latest" || models.Data[1].ID != "qwen2.5:7b" {
		t.Fatalf("GET /v1/models: %d %+v", resp.StatusCode, models)
	}

	// Whole answer. (It may queue for a moment until the device's first
	// heartbeat reports its free memory.)
	resp = openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("qwen2.5:7b", false, "who are you?"))
	var whole struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct{ Role, Content string } `json:"message"`
			Finish  string                         `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	json.NewDecoder(resp.Body).Decode(&whole)
	resp.Body.Close()
	wid := resp.Header.Get("X-Home-Harness-Workload")
	if resp.StatusCode != http.StatusOK || whole.Object != "chat.completion" || whole.ID != "chatcmpl-"+wid || whole.Model != "qwen2.5:7b" ||
		len(whole.Choices) != 1 || whole.Choices[0].Message.Role != "assistant" || whole.Choices[0].Message.Content != "from qwen" || whole.Choices[0].Finish != "stop" {
		t.Fatalf("chat.completion: %d %+v", resp.StatusCode, whole)
	}
	if whole.Usage.Prompt != 11 || whole.Usage.Completion != 3 || whole.Usage.Total != 14 {
		t.Fatalf("usage %+v", whole.Usage)
	}
	if rec, _ := m.srv.Workloads.Get(domain.WorkloadID(wid)); rec.Status.Target != b.NodeID() || qwen.lastUser != "who are you?" {
		t.Fatalf("ran on %s (want qwen-box %s); Ollama got %q", rec.Status.Target, b.NodeID(), qwen.lastUser)
	}

	// Streamed answer.
	resp = openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("llama3.2", true, "hello"))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var text strings.Builder
	chunks, done, err := readSSE(resp.Body, func(s string) { text.WriteString(s) })
	resp.Body.Close()
	if err != nil || !done || len(chunks) < 3 || chunks[0].Choices[0].Delta.Role == nil || *chunks[0].Choices[0].Delta.Role != "assistant" || text.String() != "from llama" {
		t.Fatalf("stream: done=%v text=%q chunks=%+v", done, text.String(), chunks)
	}
	last, usage := chunks[len(chunks)-2], chunks[len(chunks)-1]
	if last.Object != "chat.completion.chunk" || last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Fatalf("closing chunk %+v", last)
	}
	if usage.Usage == nil || usage.Usage.TotalTokens != 14 || len(usage.Choices) != 0 {
		t.Fatalf("usage chunk %+v", usage)
	}

	// The AI key can't use the rest of the API.
	resp = openAI(t, ctx, http.MethodGet, m.api+"/nodes", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /nodes with the AI key: %d", resp.StatusCode)
	}
}

// The text arrives while the device is still writing it, and a client
// that leaves — mid-answer or still queued — stops its work.
func TestOpenAIChatStreamsLiveAndStopsWhenTheClientLeaves(t *testing.T) {
	const addr = "127.0.0.1:19591"
	m := startAIManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"one ", "two ", "three ", "four ", "five ", "six"}, delay: 400 * time.Millisecond}
	a := startLLMAgent(t, ctx, addr, "talker", startFakeOllama(t, f), 4)
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })

	// One answer runs to the end (whole), while a second request waits its
	// turn (one generation per device) and gives up while queued.
	wholeDone := make(chan string, 1)
	go func() {
		resp := openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("llama3.2", false, "count"))
		var v struct {
			Choices []struct{ Message struct{ Content string } } `json:"choices"`
		}
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if len(v.Choices) == 1 {
			wholeDone <- v.Choices[0].Message.Content
		} else {
			wholeDone <- "status " + resp.Status
		}
	}()
	waitFor(t, 10*time.Second, func() bool {
		for _, rec := range m.srv.Workloads.List() {
			if rec.Status.State == domain.WorkloadRunning {
				return true
			}
		}
		return false
	})
	qctx, qcancel := context.WithCancel(ctx)
	queued := openAI(t, qctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("llama3.2", true, "me too"))
	qid := domain.WorkloadID(queued.Header.Get("X-Home-Harness-Workload"))
	if rec, _ := m.srv.Workloads.Get(qid); rec.Status.State != domain.WorkloadQueued {
		t.Fatalf("second chat on a busy device: %s, want QUEUED", rec.Status.State)
	}
	qcancel()
	queued.Body.Close()
	waitFor(t, 5*time.Second, func() bool { rec, _ := m.srv.Workloads.Get(qid); return rec.Status.State == domain.WorkloadCanceled })
	if got := <-wholeDone; got != "one two three four five six" {
		t.Fatalf("the whole answer: %q", got)
	}

	// Streamed: the first words arrive while the answer is still running;
	// then the client leaves.
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	resp := openAI(t, sctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("llama3.2", true, "again"))
	sid := domain.WorkloadID(resp.Header.Get("X-Home-Harness-Workload"))
	firstText := make(chan time.Time, 1)
	go readSSE(resp.Body, func(string) {
		select {
		case firstText <- time.Now():
		default:
		}
	})
	select {
	case <-firstText:
	case <-time.After(10 * time.Second):
		t.Fatal("no text streamed")
	}
	if rec, _ := m.srv.Workloads.Get(sid); rec.Status.State != domain.WorkloadRunning {
		t.Fatalf("text arrived only after the answer ended (%s): not streaming", rec.Status.State)
	}
	f.mu.Lock()
	cutBefore := f.cut
	f.mu.Unlock()
	start := time.Now()
	scancel()
	resp.Body.Close()
	waitFor(t, 5*time.Second, func() bool { rec, _ := m.srv.Workloads.Get(sid); return rec.Status.State == domain.WorkloadCanceled })
	waitFor(t, 5*time.Second, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.cut > cutBefore })
	if time.Since(start) > 5*time.Second {
		t.Fatalf("stopping took %v", time.Since(start))
	}
}

// Policy still decides: with llm.chat off, apps get OpenAI's permission
// error saying how to turn it on.
func TestOpenAIChatFollowsThePolicy(t *testing.T) {
	const addr = "127.0.0.1:19592"
	m := startAIManager(t, addr, &domain.Policy{Types: map[domain.CapabilityName]domain.TypePolicy{"llm.chat": {Enabled: false}}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := startLLMAgent(t, ctx, addr, "policy-box", startFakeOllama(t, &fakeOllama{models: []string{"llama3.2:latest"}, chunks: []string{"x"}}), 2)
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(a.NodeID()); return ok && rec.State == domain.NodeReady })
	resp := openAI(t, ctx, http.MethodPost, m.api+"/v1/chat/completions", chatBody("llama3.2", true, "hi"))
	var e struct {
		Error struct{ Type, Message string }
	}
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || e.Error.Type != "permission_error" || !strings.Contains(e.Error.Message, "harnessctl policy type llm.chat on") {
		t.Fatalf("chat while llm.chat is off: %d %+v", resp.StatusCode, e)
	}
}
