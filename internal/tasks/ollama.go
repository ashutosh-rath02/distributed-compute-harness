package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"home-harness/internal/catalog"
)

// The local-model types (roadmap item 8) talk to Ollama's HTTP API on
// this device. Where it listens is the device owner's setting
// (-ollama-url / OLLAMA_HOST), never something the manager can point
// elsewhere.

// MaxContextBytes bounds the input files given to a model as context:
// beyond the model's context window Ollama silently drops input, so a
// large limit would only pretend to work.
const MaxContextBytes = 32 << 10

type ollama struct {
	base   string
	client *http.Client
}

func newOllama(raw string) *ollama {
	return &ollama{base: NormalizeOllamaURL(raw), client: &http.Client{}}
}

// NormalizeOllamaURL turns what people put in OLLAMA_HOST into a URL to
// dial: a bind address like 0.0.0.0 (or nothing) means this machine, and
// a missing scheme or port gets Ollama's defaults.
func NormalizeOllamaURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = os.Getenv("OLLAMA_HOST")
	}
	if raw == "" {
		return "http://127.0.0.1:11434"
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "http://127.0.0.1:11434"
	}
	host, port := u.Hostname(), u.Port()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "11434"
	}
	u.Host = net.JoinHostPort(host, port)
	return strings.TrimRight(u.String(), "/")
}

type ollamaModel struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	Details    struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

func (o *ollama) tags(ctx context.Context) ([]ollamaModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Ollama not reachable at %s: %w", o.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama at %s answered %s", o.base, resp.Status)
	}
	var body struct {
		Models []ollamaModel `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("Ollama /api/tags: %w", err)
	}
	return body.Models, nil
}

func (o *ollama) available(ctx context.Context) error {
	models, err := o.tags(ctx)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return errors.New("Ollama has no models (ollama pull <model>)")
	}
	return nil
}

// ---- llm.generate

type ollamaGenerate struct{ o *ollama }

func (g ollamaGenerate) Available(ctx context.Context) error { return g.o.available(ctx) }

// Attributes advertises the models, normalized and sorted so an
// unchanged list compares equal between probes.
func (g ollamaGenerate) Attributes(ctx context.Context) map[string]string {
	models, err := g.o.tags(ctx)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, m := range models {
		if n := catalog.NormalizeModel(m.Name); n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return map[string]string{catalog.AttrModels: strings.Join(names, ",")}
}

func (g ollamaGenerate) Run(ctx context.Context, env Env) error {
	prompt := env.Params["prompt"]
	if len(env.Inputs) > 0 {
		var b strings.Builder
		for i, name := range env.Inputs {
			data, err := os.ReadFile(env.in(i))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			block := fmt.Sprintf("--- %s ---\n%s\n\n", name, data)
			if b.Len()+len(block) > MaxContextBytes {
				return fmt.Errorf("context files are over %d KiB together; a local model would silently drop the rest", MaxContextBytes>>10)
			}
			b.WriteString(block)
		}
		prompt = b.String() + prompt
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
	body, _ := json.Marshal(map[string]any{
		"model": env.Params["model"], "prompt": prompt, "system": env.Params["system"], "stream": true, "options": options,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.o.base+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.o.client.Do(req)
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
			Response     string `json:"response"`
			Done         bool   `json:"done"`
			Error        string `json:"error"`
			EvalCount    int    `json:"eval_count"`
			EvalDuration int64  `json:"eval_duration"`
			DoneReason   string `json:"done_reason"`
		}
		if err := json.Unmarshal(sc.Bytes(), &chunk); err != nil {
			return fmt.Errorf("Ollama stream: %w", err)
		}
		if chunk.Error != "" {
			return fmt.Errorf("Ollama: %s", chunk.Error)
		}
		if chunk.Response != "" {
			io.WriteString(env.Stdout, chunk.Response)
			if _, err := io.WriteString(out, chunk.Response); err != nil {
				return err
			}
		}
		if chunk.Done {
			rate := ""
			if chunk.EvalDuration > 0 {
				rate = fmt.Sprintf(", %.1f tokens/s", float64(chunk.EvalCount)/(float64(chunk.EvalDuration)/1e9))
			}
			fmt.Fprintf(env.Stderr, "%s: %d tokens%s (%s)\n", env.Params["model"], chunk.EvalCount, rate, chunk.DoneReason)
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

// ---- llm.inventory

type ollamaInventory struct{ o *ollama }

func (i ollamaInventory) Available(ctx context.Context) error { return i.o.available(ctx) }

func (i ollamaInventory) Run(ctx context.Context, env Env) error {
	models, err := i.o.tags(ctx)
	if err != nil {
		return err
	}
	type row struct {
		Name          string `json:"name"`
		SizeBytes     int64  `json:"sizeBytes"`
		Family        string `json:"family,omitempty"`
		ParameterSize string `json:"parameterSize,omitempty"`
		Quantization  string `json:"quantization,omitempty"`
	}
	rows := make([]row, 0, len(models))
	for _, m := range models {
		rows = append(rows, row{catalog.NormalizeModel(m.Name), m.Size, m.Details.Family, m.Details.ParameterSize, m.Details.QuantizationLevel})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
