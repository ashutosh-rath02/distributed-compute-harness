package manager

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func chatRequest(t *testing.T, body string) openAIChatRequest {
	t.Helper()
	var req openAIChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestChatParamsTranslateOpenAIRequests(t *testing.T) {
	p, err := chatParams(chatRequest(t, `{
		"model": "Llama3.2", "stream": true, "temperature": 0.3, "max_tokens": 50, "max_completion_tokens": 80, "seed": 9,
		"tools": [], "n": 1, "user": "ignored", "presence_penalty": 0.5,
		"messages": [
			{"role": "developer", "content": "Be brief."},
			{"role": "user", "content": [{"type": "text", "text": "line one"}, {"type": "text", "text": "line two"}]},
			{"role": "assistant", "content": null},
			{"role": "user", "content": "thanks"}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []chatMessage
	json.Unmarshal([]byte(p["messages"]), &msgs)
	want := []chatMessage{{"system", "Be brief."}, {"user", "line one\nline two"}, {"assistant", ""}, {"user", "thanks"}}
	if len(msgs) != len(want) {
		t.Fatalf("messages %+v", msgs)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Fatalf("message %d: %+v, want %+v", i, msgs[i], want[i])
		}
	}
	// max_completion_tokens (the newer name) wins over max_tokens.
	if p["model"] != "llama3.2:latest" || p["temperature"] != "0.3" || p["max_tokens"] != "80" || p["seed"] != "9" {
		t.Fatalf("params %v", p)
	}
	if p, _ := chatParams(chatRequest(t, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)); len(p) != 2 {
		t.Fatalf("unset options must stay unset (the model's own defaults): %v", p)
	}

	for body, want := range map[string]string{
		`{"messages":[{"role":"user","content":"x"}]}`:                                               "model is required",
		`{"model":"m","messages":[]}`:                                                                "messages is empty",
		`{"model":"m","n":2,"messages":[{"role":"user","content":"x"}]}`:                             "n must be 1",
		`{"model":"m","tools":[{"type":"function"}],"messages":[{"role":"user","content":"x"}]}`:     "tools",
		`{"model":"m","messages":[{"role":"tool","content":"x"}]}`:                                   `role "tool"`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`: "only text",
		`{"model":"m","messages":[{"role":"user","content":42}]}`:                                    "string or a list",
		`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 70000) + `"}]}`:  "at most 65536 fit",
	} {
		if _, err := chatParams(chatRequest(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			b := body
			if len(b) > 80 {
				b = b[:80]
			}
			t.Errorf("%s: %v (want %q)", b, err, want)
		}
	}
}

func TestCompleteRunesHoldsBackAPartialCharacter(t *testing.T) {
	s := "été"
	for cut, want := range map[int]string{len(s): "été", len(s) - 1: "ét", 1: "", 0: ""} {
		if got := completeRunes(s[:cut]); got != want {
			t.Errorf("completeRunes(%q) = %q, want %q", s[:cut], got, want)
		}
	}
}

const testAIKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// chatNode adds a READY node offering capabilities, each with models.
func chatNode(t *testing.T, s *Server, id domain.NodeID, state domain.NodeState, caps map[domain.CapabilityName]string) {
	t.Helper()
	s.Registry.Upsert(testNode(id), &fakeConn{tag: string(id)})
	s.Registry.SetState(id, state)
	var list []domain.Capability
	for name, models := range caps {
		list = append(list, domain.Capability{Name: name, Version: "1", Attributes: map[string]string{"models": models}})
	}
	s.Registry.UpdateResources(id, nil, list)
}

// The AI key opens the chat API and nothing else; the operator's
// credentials open it too.
func TestAIKeyOpensOnlyTheChatAPI(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken, AIKey: testAIKey, PairingToken: "permanent-pairing-secret"})
	h := s.NewHTTPHandler()
	for _, bearer := range []string{testAIKey, testOperatorToken, dashboardSession(testOperatorToken)} {
		if rec := operatorRequest(t, h, http.MethodGet, "/v1/models", bearer, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/models with %.8s...: %d %s", bearer, rec.Code, rec.Body)
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/nodes", ""},
		{http.MethodGet, "/ai-info", ""},
		{http.MethodGet, "/join-info", ""},
		{http.MethodPost, "/workloads", `{"capability":"llm.chat","params":{"model":"m","messages":"[]"}}`},
		{http.MethodGet, "/workloads", ""},
		{http.MethodPost, "/join-window", "{}"},
	} {
		if rec := operatorRequest(t, h, route.method, route.path, testAIKey, route.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with the AI key: %d, want 401", route.method, route.path, rec.Code)
		}
	}
	// Wrong or missing keys get OpenAI's error shape, which client
	// libraries turn into a proper "invalid API key" error.
	for _, bearer := range []string{"", "sk-wrong", "pairing-token"} {
		rec := operatorRequest(t, h, http.MethodPost, "/v1/chat/completions", bearer, `{}`)
		var v struct {
			Error struct{ Type, Code, Message string }
		}
		json.Unmarshal(rec.Body.Bytes(), &v)
		if rec.Code != http.StatusUnauthorized || v.Error.Code != "invalid_api_key" || v.Error.Message == "" {
			t.Errorf("chat with %q: %d %s", bearer, rec.Code, rec.Body)
		}
	}
	// The operator reads the key and base URL to set apps up.
	rec := operatorRequest(t, h, http.MethodGet, "/ai-info", testOperatorToken, "")
	var info struct {
		BaseURL string `json:"baseUrl"`
		APIKey  string `json:"apiKey"`
	}
	json.Unmarshal(rec.Body.Bytes(), &info)
	if rec.Code != http.StatusOK || info.BaseURL != "http://127.0.0.1:7421/v1" || info.APIKey != testAIKey {
		t.Fatalf("GET /ai-info: %d %s", rec.Code, rec.Body)
	}
	// No AI key configured: only the operator credentials work.
	h = NewServer(nil, nil, Config{OperatorToken: testOperatorToken}).NewHTTPHandler()
	if rec := operatorRequest(t, h, http.MethodGet, "/v1/models", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty key accepted: %d", rec.Code)
	}
}

func TestChatModelsComeFromReadyDevicesOfferingChat(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	chatNode(t, s, "n1", domain.NodeReady, map[domain.CapabilityName]string{"llm.chat": "llama3.2:latest,qwen2.5:7b", "llm.generate": "llama3.2:latest,qwen2.5:7b"})
	chatNode(t, s, "n2", domain.NodeReady, map[domain.CapabilityName]string{"llm.chat": "gemma3:1b"})
	chatNode(t, s, "n3", domain.NodeOffline, map[domain.CapabilityName]string{"llm.chat": "mistral:latest"})
	chatNode(t, s, "old", domain.NodeReady, map[domain.CapabilityName]string{"llm.generate": "phi3:latest"}) // an agent from before llm.chat
	h := s.NewHTTPHandler()
	rec := operatorRequest(t, h, http.MethodGet, "/v1/models", "", "")
	var v struct {
		Object string
		Data   []struct{ ID, Object, OwnedBy string }
	}
	json.Unmarshal(rec.Body.Bytes(), &v)
	var ids []string
	for _, m := range v.Data {
		ids = append(ids, m.ID)
		if m.Object != "model" {
			t.Fatalf("model object %+v", m)
		}
	}
	if v.Object != "list" || strings.Join(ids, ",") != "gemma3:1b,llama3.2:latest,qwen2.5:7b" {
		t.Fatalf("GET /v1/models: %s", rec.Body)
	}
	// The dashboard's /models lists every model, chat or not, once per device.
	rec = operatorRequest(t, h, http.MethodGet, "/models", "", "")
	var models []struct {
		Model string
		Nodes []struct{ ID string }
	}
	json.Unmarshal(rec.Body.Bytes(), &models)
	var got []string
	for _, m := range models {
		got = append(got, m.Model+"="+string(rune('0'+len(m.Nodes))))
	}
	if strings.Join(got, ",") != "gemma3:1b=1,llama3.2:latest=1,phi3:latest=1,qwen2.5:7b=1" {
		t.Fatalf("GET /models: %v", got)
	}

	// A model no ready device has: OpenAI's 404, naming what there is.
	rec = operatorRequest(t, h, http.MethodPost, "/v1/chat/completions", "", `{"model":"mistral","messages":[{"role":"user","content":"hi"}]}`)
	var e struct {
		Error struct{ Type, Code, Message string }
	}
	json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusNotFound || e.Error.Code != "model_not_found" || !strings.Contains(e.Error.Message, "gemma3:1b, llama3.2:latest, qwen2.5:7b") {
		t.Fatalf("unknown model: %d %s", rec.Code, rec.Body)
	}
	rec = operatorRequest(t, h, http.MethodPost, "/v1/chat/completions", "", `{"model":"gemma3:1b","messages":[{"role":"tool","content":"x"}]}`)
	if json.Unmarshal(rec.Body.Bytes(), &e); rec.Code != http.StatusBadRequest || e.Error.Type != "invalid_request_error" {
		t.Fatalf("bad request: %d %s", rec.Code, rec.Body)
	}
	rec = operatorRequest(t, h, http.MethodPost, "/v1/chat/completions", "", `{"model":`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("broken JSON: %d %s", rec.Code, rec.Body)
	}
}
