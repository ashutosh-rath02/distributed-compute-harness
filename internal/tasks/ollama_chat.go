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
	"strconv"

	"home-harness/internal/catalog"
)

// ---- llm.chat: one turn of a conversation, the way chat apps send it
// (the manager's OpenAI-compatible API turns requests into this). Ollama
// applies the model's own chat template to the messages.

// ChatMessage is one message of a conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ParseChatMessages reads and checks the llm.chat "messages" parameter:
// a JSON array of {role, content}, roles system, user or assistant.
func ParseChatMessages(raw string) ([]ChatMessage, error) {
	var msgs []ChatMessage
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil, fmt.Errorf("messages must be a JSON array of {role, content}: %w", err)
	}
	if len(msgs) == 0 {
		return nil, errors.New("messages is empty")
	}
	for i, m := range msgs {
		switch m.Role {
		case "system", "user", "assistant":
		default:
			return nil, fmt.Errorf("message %d: role %q (want system, user or assistant)", i, m.Role)
		}
	}
	return msgs, nil
}

type ollamaChat struct{ o *ollama }

func (c ollamaChat) Available(ctx context.Context) error { return c.o.available(ctx) }

// Attributes advertises the same models as llm.generate: placement
// matches the requested model against them.
func (c ollamaChat) Attributes(ctx context.Context) map[string]string {
	return ollamaGenerate{c.o}.Attributes(ctx)
}

func (c ollamaChat) Run(ctx context.Context, env Env) error {
	msgs, err := ParseChatMessages(env.Params["messages"])
	if err != nil {
		return err
	}
	options := map[string]any{}
	if v, err := strconv.ParseFloat(env.Params["temperature"], 64); err == nil {
		options["temperature"] = v
	}
	if v, _ := strconv.Atoi(env.Params["max_tokens"]); v > 0 {
		options["num_predict"] = v
	}
	if v, _ := strconv.Atoi(env.Params["seed"]); v > 0 {
		options["seed"] = v
	}
	// Placement matched the normalized name; Ollama gets the exact name it
	// listed.
	model := env.Params["model"]
	if models, err := c.o.tags(ctx); err == nil {
		want, found := catalog.NormalizeModel(model), false
		for _, m := range models {
			if catalog.NormalizeModel(m.Name) == want {
				model, found = m.Name, true
				break
			}
		}
		if !found {
			return fmt.Errorf("model %q isn't on this device any more", env.Params["model"])
		}
	}
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "stream": true, "options": options})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.o.base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.o.client.Do(req)
	if err != nil {
		return fmt.Errorf("Ollama not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("Ollama: %s", e.Error)
		}
		return fmt.Errorf("Ollama answered %s", resp.Status)
	}
	out, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	defer out.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var chunk struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done            bool   `json:"done"`
			Error           string `json:"error"`
			DoneReason      string `json:"done_reason"`
			PromptEvalCount int    `json:"prompt_eval_count"`
			EvalCount       int    `json:"eval_count"`
			EvalDuration    int64  `json:"eval_duration"`
		}
		if err := json.Unmarshal(sc.Bytes(), &chunk); err != nil {
			return fmt.Errorf("Ollama stream: %w", err)
		}
		if chunk.Error != "" {
			return fmt.Errorf("Ollama: %s", chunk.Error)
		}
		if chunk.Message.Content != "" {
			io.WriteString(env.Stdout, chunk.Message.Content)
			if _, err := io.WriteString(out, chunk.Message.Content); err != nil {
				return err
			}
		}
		if chunk.Done {
			rate := ""
			if chunk.EvalDuration > 0 {
				rate = fmt.Sprintf(", %.1f tokens/s", float64(chunk.EvalCount)/(float64(chunk.EvalDuration)/1e9))
			}
			// The manager's chat API reads usage and the finish reason from
			// this line.
			fmt.Fprintf(env.Stderr, "%s: %d prompt tokens, %d tokens%s (%s)\n", env.Params["model"], chunk.PromptEvalCount, chunk.EvalCount, rate, chunk.DoneReason)
			return out.Close()
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Ollama stream: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("Ollama ended the answer without finishing it")
}
