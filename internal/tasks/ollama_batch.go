package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"home-harness/internal/catalog"
)

// ---- llm.classify and llm.embed: the per-file steps of a batch AI job
// over a folder (one task per file, spread over the devices with the
// model; report.collect joins the results).

// embeds reports whether m makes embeddings. Only a model that says so
// counts: an older Ollama lists no capabilities, and a text model asked
// for embeddings gives poor ones (or an error).
func (m ollamaModel) embeds() bool { return slices.Contains(m.Capabilities, "embedding") }

// availableWith reports whether Ollama answers and has a model keep
// accepts (none says what is missing).
func (o *ollama) availableWith(ctx context.Context, keep func(ollamaModel) bool, none string) error {
	models, err := o.tags(ctx)
	if err != nil {
		return err
	}
	for _, m := range models {
		if keep(m) {
			return nil
		}
	}
	return errors.New(none)
}

// listedName is model as Ollama lists it (its own case and tag):
// placement matched the normalized name.
func (o *ollama) listedName(ctx context.Context, model string) (string, error) {
	models, err := o.tags(ctx)
	if err != nil {
		return "", err
	}
	want := catalog.NormalizeModel(model)
	for _, m := range models {
		if catalog.NormalizeModel(m.Name) == want {
			return m.Name, nil
		}
	}
	return "", fmt.Errorf("model %q isn't on this device any more", model)
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// readText reads a text file for a model: UTF-8, not empty, and at most
// MaxContextBytes — beyond a model's context window the rest would be
// silently dropped (or refused), so a larger file would only pretend to
// work.
func readText(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxContextBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if len(data) > MaxContextBytes {
		return "", fmt.Errorf("%s is over %d KiB, more than a local model takes in at once (split it)", name, MaxContextBytes>>10)
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s isn't UTF-8 text", name)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", fmt.Errorf("%s is empty", name)
	}
	return string(data), nil
}

// postOllama sends body to Ollama's endpoint and decodes its whole JSON
// answer into out (at most 64 MiB: embeddings of a few files).
func (o *ollama) postOllama(ctx context.Context, endpoint string, body any, out any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Ollama not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch {
		case json.Unmarshal(raw, &e) == nil && e.Error != "":
			return fmt.Errorf("Ollama: %s", e.Error)
		case resp.StatusCode == http.StatusNotFound:
			// Not an error of Ollama's own (that names a missing model):
			// the endpoint itself is missing, as before /api/embed existed.
			return fmt.Errorf("this device's Ollama has no %s (update Ollama)", endpoint)
		}
		return fmt.Errorf("Ollama answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Ollama %s: %w", endpoint, err)
	}
	return nil
}

// ---- llm.classify

type ollamaClassify struct{ o *ollama }

func (c ollamaClassify) Available(ctx context.Context) error {
	return c.o.availableWith(ctx, ollamaModel.generates, "Ollama has no model that writes text")
}

// Attributes: the models that write text (as llm.generate's).
func (c ollamaClassify) Attributes(ctx context.Context) map[string]string {
	return c.o.modelAttrs(ctx, ollamaModel.generates)
}

// classifySystem is the instruction every classification starts with.
const classifySystem = "You sort documents into categories. Read the document and answer with the one label from the list that fits it best."

func (c ollamaClassify) Run(ctx context.Context, env Env) error {
	labels, err := catalog.ParseLabels(env.Params["labels"])
	if err != nil {
		return err
	}
	text, err := readText(env.in(0), env.Inputs[0])
	if err != nil {
		return err
	}
	model, err := c.o.listedName(ctx, env.Params["model"])
	if err != nil {
		return err
	}
	// The answer's shape is a JSON schema whose only value is one of the
	// labels: Ollama constrains the model to it. It is checked again below
	// all the same.
	format := map[string]any{
		"type":       "object",
		"properties": map[string]any{"label": map[string]any{"type": "string", "enum": labels}},
		"required":   []string{"label"},
	}
	user := fmt.Sprintf("Labels: %s\n\nDocument %q:\n<<<\n%s\n>>>\n\nWhich one label fits this document best?", strings.Join(labels, ", "), env.Inputs[0], text)
	var answer struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
		DoneReason      string `json:"done_reason"`
	}
	err = c.o.postOllama(ctx, "/api/chat", map[string]any{
		"model": model, "stream": false, "format": format,
		"messages": []ChatMessage{{Role: "system", Content: classifySystem}, {Role: "user", Content: user}},
		// Deterministic, and room for the longest label as JSON.
		"options": map[string]any{"temperature": 0, "num_predict": 256},
	}, &answer)
	if err != nil {
		return err
	}
	label, err := pickLabel(answer.Message.Content, labels)
	if err != nil {
		return err
	}
	out, _ := json.Marshal(map[string]string{"file": env.Inputs[0], "label": label})
	if err := os.WriteFile(env.out(0), append(out, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s: %s\n", env.Inputs[0], label)
	fmt.Fprintf(env.Stderr, "%s: %d prompt tokens, %d tokens (%s)\n", env.Params["model"], answer.PromptEvalCount, answer.EvalCount, answer.DoneReason)
	return nil
}

// pickLabel reads the model's answer, {"label": "..."}, and returns the
// label it names, exactly as the operator wrote it. Anything else — not
// JSON, no label, a label that isn't one of them — is refused: the format
// should rule it out, but the answer is the model's, so it is checked.
func pickLabel(answer string, labels []string) (string, error) {
	var a struct {
		Label *string `json:"label"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(answer)), &a); err != nil || a.Label == nil {
		return "", fmt.Errorf("the model didn't answer with a label: %q", clip(answer, 200))
	}
	got := strings.TrimSpace(*a.Label)
	for _, l := range labels {
		if got == l {
			return l, nil
		}
	}
	for _, l := range labels {
		if strings.EqualFold(got, l) {
			return l, nil
		}
	}
	return "", fmt.Errorf("the model answered %q, which isn't one of the labels", clip(*a.Label, 200))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}

// ---- llm.embed

type ollamaEmbed struct{ o *ollama }

func (e ollamaEmbed) Available(ctx context.Context) error {
	return e.o.availableWith(ctx, ollamaModel.embeds, "Ollama has no embedding model (e.g. ollama pull embeddinggemma)")
}

// Attributes: only the embedding models — llm.embed's own list, which
// placement matches its model parameter against.
func (e ollamaEmbed) Attributes(ctx context.Context) map[string]string {
	return e.o.modelAttrs(ctx, ollamaModel.embeds)
}

// Embedding is one entry of embeddings.json.
type Embedding struct {
	Name   string    `json:"name"`
	Model  string    `json:"model"`
	Vector []float32 `json:"vector"`
}

func (e ollamaEmbed) Run(ctx context.Context, env Env) error {
	model, err := e.o.listedName(ctx, env.Params["model"])
	if err != nil {
		return err
	}
	entries := make([]Embedding, 0, len(env.Inputs))
	for i, name := range env.Inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, err := readText(env.in(i), name)
		if err != nil {
			return err
		}
		var answer struct {
			Embeddings      [][]float32 `json:"embeddings"`
			PromptEvalCount int         `json:"prompt_eval_count"`
		}
		// truncate false: a text longer than the model's context is an
		// error rather than an embedding of only its beginning.
		err = e.o.postOllama(ctx, "/api/embed", map[string]any{"model": model, "input": text, "truncate": false}, &answer)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(answer.Embeddings) != 1 || len(answer.Embeddings[0]) == 0 {
			return fmt.Errorf("%s: Ollama answered %d embeddings, want 1", name, len(answer.Embeddings))
		}
		entries = append(entries, Embedding{Name: name, Model: catalog.NormalizeModel(env.Params["model"]), Vector: answer.Embeddings[0]})
		fmt.Fprintf(env.Stdout, "%s: %d numbers (%d tokens)\n", name, len(answer.Embeddings[0]), answer.PromptEvalCount)
	}
	return writeEmbeddings(env.out(0), entries)
}

// writeEmbeddings writes entries as a JSON array, one entry per line.
func writeEmbeddings(path string, entries []Embedding) error {
	var b bytes.Buffer
	b.WriteString("[\n")
	for i, en := range entries {
		line, err := json.Marshal(en)
		if err != nil {
			return err
		}
		b.Write(line)
		if i < len(entries)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return os.WriteFile(path, b.Bytes(), 0o600)
}
