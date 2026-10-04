package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Local models served by something other than Ollama: a split session's
// llama-server (llama.cpp, the model's layers spread over several
// devices) speaks OpenAI's chat API on this device's loopback. While it
// runs, llm.chat offers its model and sends that model's chats there;
// everything else about a chat (placement, queueing, the manager's /v1)
// stays as it is.

type localModels struct {
	mu sync.Mutex
	m  map[string]localModel // normalized model name -> where and as what
}

type localModel struct {
	base  string // OpenAI base URL
	alias string // the name the server knows it by, as given
}

func (l *localModels) get(model string) (localModel, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lm, ok := l.m[catalog.NormalizeModel(model)]
	return lm, ok
}

func (l *localModels) names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.m))
	for n := range l.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ServeModel has llm.chat offer model, answered by the OpenAI-compatible
// server at base (e.g. http://127.0.0.1:41234), until the returned func
// is called.
func (r *Registry) ServeModel(model, base string) func() {
	l := &r.ollama.local
	name := catalog.NormalizeModel(model)
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]localModel{}
	}
	l.m[name] = localModel{base: strings.TrimRight(base, "/"), alias: model}
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.m, name)
		l.mu.Unlock()
	}
}

// Register adds or replaces the handler for a catalog type (the agent's
// own handlers, which need more than an Env, e.g. split sessions).
func (r *Registry) Register(name string, h Handler) {
	r.handlers[domain.CapabilityName(name)] = h
}

// openAIChat streams one chat turn from an OpenAI-compatible server,
// writing the reply like the Ollama path does: text to stdout and
// response.txt, a stats line to stderr for the manager's usage figures.
func openAIChat(ctx context.Context, env Env, base, model string, msgs []ChatMessage, options map[string]any) error {
	req := map[string]any{"model": model, "messages": msgs, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if v, ok := options["temperature"]; ok {
		req["temperature"] = v
	}
	if v, ok := options["num_predict"]; ok {
		req["max_tokens"] = v
	}
	if v, ok := options["seed"]; ok {
		req["seed"] = v
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return fmt.Errorf("the split model's server isn't answering: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the split model's server answered %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	out, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	defer out.Close()
	finish, prompt, completion := "", 0, 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			fmt.Fprintf(env.Stderr, "%s: %d prompt tokens, %d tokens (%s)\n", env.Params["model"], prompt, completion, finish)
			return out.Close()
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("the split model's server: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("the split model's server: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			prompt, completion = chunk.Usage.Prompt, chunk.Usage.Completion
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				io.WriteString(env.Stdout, c.Delta.Content)
				if _, err := io.WriteString(out, c.Delta.Content); err != nil {
					return err
				}
			}
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("the split model's server: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("the split model's server ended the answer without finishing it")
}
