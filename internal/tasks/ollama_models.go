package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"home-harness/internal/catalog"
)

// ---- llm.pull / llm.remove: manage the models of the device's own
// Ollama from the manager (the dashboard's "AI models" card). Both name
// their device: models are stored per device.

type ollamaPull struct{ o *ollama }

// Available while Ollama answers, even with no models yet — the first
// pull is when it matters most.
func (p ollamaPull) Available(ctx context.Context) error {
	_, err := p.o.tags(ctx)
	return err
}

// pullStep is how far a layer's download must move before progress is
// printed again: a multi-gigabyte pull reports many times a second, and
// the output kept for a task is capped (domain.OutputCapBytes).
const pullStep = 5

func (p ollamaPull) Run(ctx context.Context, env Env) error {
	// The model list changed (or may have): the agent's next look at it
	// must ask Ollama again, not reuse what it cached before the pull.
	defer p.o.invalidate()
	model := strings.TrimSpace(env.Params["model"])
	// "model" is Ollama's field; "name" is what versions before 0.4 read.
	body, _ := json.Marshal(map[string]any{"model": model, "name": model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.o.base+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.o.client.Do(req)
	if err != nil {
		return fmt.Errorf("Ollama not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ollamaError(resp)
	}
	lastStatus, lastPct := "", -pullStep
	var total int64
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var line struct {
			Status    string `json:"status"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return fmt.Errorf("Ollama pull: %w", err)
		}
		if line.Error != "" {
			return fmt.Errorf("Ollama: %s", line.Error)
		}
		if line.Status != lastStatus {
			fmt.Fprintln(env.Stdout, line.Status)
			lastStatus, lastPct = line.Status, -pullStep
		}
		if line.Total > 0 {
			pct := int(line.Completed * 100 / line.Total)
			if pct >= lastPct+pullStep || (pct == 100 && lastPct < 100) {
				fmt.Fprintf(env.Stdout, "  %d%% (%s of %s)\n", pct, mib(line.Completed), mib(line.Total))
				lastPct = pct
			}
			if line.Total > total {
				total = line.Total
			}
		}
		if line.Status == "success" {
			fmt.Fprintf(env.Stderr, "pulled %s\n", model)
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Ollama pull: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("Ollama stopped before %s finished downloading", model)
}

func mib(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MB", n>>20)
}

// ollamaError reads Ollama's {"error": "..."} answer.
func ollamaError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return fmt.Errorf("Ollama: %s", e.Error)
	}
	return fmt.Errorf("Ollama answered %s", resp.Status)
}

type ollamaRemove struct{ o *ollama }

func (r ollamaRemove) Available(ctx context.Context) error { return r.o.available(ctx) }

// Attributes: the models, which the model parameter must be one of.
func (r ollamaRemove) Attributes(ctx context.Context) map[string]string {
	return ollamaGenerate{r.o}.Attributes(ctx)
}

func (r ollamaRemove) Run(ctx context.Context, env Env) error {
	defer r.o.invalidate()
	model := env.Params["model"]
	if models, err := r.o.tags(ctx); err == nil {
		want, found := catalog.NormalizeModel(model), false
		for _, m := range models {
			if catalog.NormalizeModel(m.Name) == want {
				model, found = m.Name, true
				break
			}
		}
		if !found {
			return fmt.Errorf("model %q isn't on this device", env.Params["model"])
		}
	}
	body, _ := json.Marshal(map[string]any{"model": model, "name": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, r.o.base+"/api/delete", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.o.client.Do(req)
	if err != nil {
		return fmt.Errorf("Ollama not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ollamaError(resp)
	}
	fmt.Fprintf(env.Stdout, "removed %s\n", model)
	return nil
}
