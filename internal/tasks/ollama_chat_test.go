package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"home-harness/internal/catalog"
)

func chatEnv(t *testing.T, params map[string]string) (Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	ty, _ := catalog.Lookup("llm.chat")
	canon, outputs, err := ty.Compile(params, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	return Env{Dir: t.TempDir(), Params: canon, Outputs: outputs, Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func TestChatStreamsTheReplyAndPassesTheConversation(t *testing.T) {
	f := &fakeOllama{models: []string{"Llama3.2:latest"}, chunks: []string{"Hi ", "there", " été"}}
	srv := f.server(t)
	h, ok := NewRegistry(Options{OllamaURL: srv.URL}).Lookup("llm.chat")
	if !ok {
		t.Fatal("llm.chat not registered")
	}
	msgs := `[{"role":"system","content":"Be brief."},{"role":"user","content":"hello"},{"role":"assistant","content":"hey"},{"role":"user","content":"again"}]`
	env, stdout, stderr := chatEnv(t, map[string]string{"model": "llama3.2", "messages": msgs, "temperature": "0.2", "max_tokens": "64", "seed": "7"})
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env.out(0))
	if stdout.String() != "Hi there été" || string(b) != stdout.String() {
		t.Fatalf("stdout %q, response.txt %q", stdout.String(), b)
	}
	if !strings.Contains(stderr.String(), "12 prompt tokens, 7 tokens, 7.0 tokens/s (stop)") {
		t.Fatalf("stats line %q", stderr.String())
	}
	got := f.chats[0]
	// Ollama gets the exact name it listed, and the whole conversation.
	if got.Model != "Llama3.2:latest" || len(got.Messages) != 4 || got.Messages[3] != (ChatMessage{"user", "again"}) || got.Messages[0].Role != "system" {
		t.Fatalf("sent %+v", got)
	}
	if got.Options["temperature"] != 0.2 || got.Options["num_predict"] != 64.0 || got.Options["seed"] != 7.0 {
		t.Fatalf("options %v", got.Options)
	}
	// Unset temperature and max tokens leave the model's own defaults.
	env, _, _ = chatEnv(t, map[string]string{"model": "llama3.2", "messages": `[{"role":"user","content":"x"}]`})
	if err := h.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if o := f.chats[1].Options; len(o) != 0 {
		t.Fatalf("options with nothing set: %v", o)
	}
}

func TestChatRefusesBadConversationsAndHonorsCancel(t *testing.T) {
	f := &fakeOllama{models: []string{"m:latest"}, chunks: []string{"a", "b", "c", "d"}, delay: 300 * time.Millisecond, reason: "length"}
	srv := f.server(t)
	h, _ := NewRegistry(Options{OllamaURL: srv.URL}).Lookup("llm.chat")
	for msgs, want := range map[string]string{
		`not json`:                        "JSON array",
		`[]`:                              "empty",
		`[{"role":"tool","content":"x"}]`: `role "tool"`,
	} {
		env, _, _ := chatEnv(t, map[string]string{"model": "m", "messages": msgs})
		if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("messages %s: %v (want %q)", msgs, err, want)
		}
	}
	env, _, _ := chatEnv(t, map[string]string{"model": "gone", "messages": `[{"role":"user","content":"x"}]`})
	if err := h.Run(context.Background(), env); err == nil || !strings.Contains(err.Error(), `"gone" isn't on this device`) {
		t.Fatalf("missing model: %v", err)
	}
	env, stdout, _ := chatEnv(t, map[string]string{"model": "m", "messages": `[{"role":"user","content":"x"}]`})
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.Run(ctx, env)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second || stdout.String() != "a" {
		t.Fatalf("cancel mid-stream: %v after %v, got %q", err, time.Since(start), stdout.String())
	}
	// A reply cut off by the token limit says so on the stats line.
	env, _, stderr := chatEnv(t, map[string]string{"model": "m", "messages": `[{"role":"user","content":"x"}]`})
	if err := h.Run(context.Background(), env); err != nil || !strings.HasSuffix(strings.TrimSpace(stderr.String()), "(length)") {
		t.Fatalf("length: %v %q", err, stderr.String())
	}
}

func TestChatIsAdvertisedWithTheModels(t *testing.T) {
	f := &fakeOllama{}
	srv := f.server(t)
	reg := NewRegistry(Options{OllamaURL: srv.URL, tagsTTL: -1})
	attrs := func() (map[string]string, bool) {
		for _, c := range reg.Capabilities(context.Background()) {
			if c.Name == "llm.chat" {
				return c.Attributes, true
			}
		}
		return nil, false
	}
	if _, ok := attrs(); ok {
		t.Fatal("llm.chat advertised while Ollama has no models")
	}
	f.setModels("Gemma3:1B", "llama3.2")
	if a, ok := attrs(); !ok || a[catalog.AttrModels] != "gemma3:1b,llama3.2:latest" {
		t.Fatalf("llm.chat attributes %v (advertised %v)", a, ok)
	}
}

// A structured answer: "format" reaches Ollama as given (a schema keeps
// its key order), and llama-server as OpenAI's response_format.
func TestChatPassesTheAnswerFormat(t *testing.T) {
	f := &fakeOllama{models: []string{"gemma3:1b"}, chunks: []string{`{"a":1}`}}
	srv := f.server(t)
	reg := NewRegistry(Options{OllamaURL: srv.URL})
	h, _ := reg.Lookup("llm.chat")
	msgs := `[{"role":"user","content":"plan"}]`
	schema := `{"type":"object", "properties":{"zeta":{"type":"string"},"alpha":{"type":"integer"}}}`
	for i, format := range []string{"", "json", schema} {
		env, _, _ := chatEnv(t, map[string]string{"model": "gemma3:1b", "messages": msgs, "format": format})
		if err := h.Run(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		got := string(f.chats[i].Format)
		want := map[string]string{"": "", "json": `"json"`, schema: `{"type":"object","properties":{"zeta":{"type":"string"},"alpha":{"type":"integer"}}}`}[format]
		if got != want {
			t.Fatalf("format %q reached Ollama as %q, want %q", format, got, want)
		}
	}
	if a := (ollamaChat{reg.ollama}).Attributes(context.Background()); a[catalog.AttrChatFormat] == "" {
		t.Fatalf("llm.chat doesn't advertise the format parameter: %v", a)
	}

	var bodies []map[string]any
	llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"{}"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer llama.Close()
	for _, format := range []string{"", "json", schema} {
		env, _, _ := chatEnv(t, map[string]string{"model": "gemma3:1b", "messages": msgs, "format": format})
		raw, _ := chatFormat(env.Params["format"])
		if err := openAIChat(context.Background(), env, llama.URL, "split", []ChatMessage{{"user", "plan"}}, nil, raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, set := bodies[0]["response_format"]; set {
		t.Fatalf("free text sent a response_format: %v", bodies[0])
	}
	if rf, _ := json.Marshal(bodies[1]["response_format"]); string(rf) != `{"type":"json_object"}` {
		t.Fatalf("json: %s", rf)
	}
	rf := bodies[2]["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("schema: %v", rf)
	}
	if _, err := chatFormat("yaml please"); err == nil {
		t.Fatal("a format that is neither json nor a schema")
	}
}
