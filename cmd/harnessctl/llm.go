package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

// Local AI models on the fleet (llm.generate / llm.inventory, Ollama).

type modelRow struct {
	Model string `json:"model"`
	Nodes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"nodes"`
}

func (c *apiClient) models() ([]modelRow, error) {
	var rows []modelRow
	return rows, c.get("/models", &rows)
}

func (c *apiClient) cmdModels() error {
	rows, err := c.models()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No device offers a local model yet. On one with enough memory: install Ollama, run")
		fmt.Println("  ollama pull llama3.2:3b")
		fmt.Println("and within about 30 seconds its agent reports it (check -ollama-url / OLLAMA_HOST if not).")
		return nil
	}
	fmt.Printf("%-40s %s\n", "MODEL", "ON")
	for _, r := range rows {
		var names []string
		for _, n := range r.Nodes {
			name := n.Name
			if name == "" {
				name = n.ID
			}
			names = append(names, name)
		}
		fmt.Printf("%-40s %s\n", r.Model, strings.Join(names, ", "))
	}
	return nil
}

// cmdAI prints what an app needs to use the fleet's models through the
// manager's OpenAI-compatible API.
func (c *apiClient) cmdAI() error {
	var info struct {
		BaseURL string   `json:"baseUrl"`
		APIKey  string   `json:"apiKey"`
		Models  []string `json:"models"`
	}
	if err := c.get("/ai-info", &info); err != nil {
		return err
	}
	fmt.Println("Apps that speak OpenAI's chat API can use your devices' models with:")
	fmt.Println("  base URL: " + info.BaseURL)
	fmt.Println("  API key:  " + info.APIKey)
	if len(info.Models) == 0 {
		fmt.Println("  models:   none yet (install Ollama on a device and pull one: ollama pull llama3.2)")
	} else {
		fmt.Println("  models:   " + strings.Join(info.Models, ", "))
	}
	fmt.Println("The key opens only the chat API (/v1). It answers on this computer only.")
	return nil
}

// cmdAsk runs a prompt on a local model somewhere in the fleet and
// prints the answer as it is generated. Ctrl-C cancels it.
func cmdAsk(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	model := fs.String("model", "", "model to use (default: the only one the fleet has; see harnessctl models)")
	target := fs.String("target", "", "run on this node")
	system := fs.String("system", "", "system instructions")
	maxTokens := fs.Int("max-tokens", 0, "longest answer, in tokens (default 512)")
	var inputs fileList
	fs.Var(&inputs, "in", "text file to give the model as context, repeatable")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: harnessctl ask [-model M] [-in FILE ...] [-target ID] "question"`)
	}
	pos, err := parseTyped(fs, args)
	if err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(pos, " "))
	if prompt == "" {
		fs.Usage()
		return errors.New("missing the question")
	}
	if *model == "" {
		rows, err := c.models()
		if err != nil {
			return err
		}
		switch {
		case len(rows) == 0:
			return errors.New("no device offers a local model yet (see harnessctl models)")
		case len(rows) > 1:
			var all []string
			for _, r := range rows {
				all = append(all, r.Model)
			}
			fmt.Fprintf(os.Stderr, "(using %s; the fleet also has %s — pick one with -model)\n", rows[0].Model, strings.Join(all[1:], ", "))
		}
		*model = rows[0].Model
	}
	in, err := c.resolveInputs(inputs)
	if err != nil {
		return err
	}
	params := map[string]string{"model": *model, "prompt": prompt}
	if *system != "" {
		params["system"] = *system
	}
	if *maxTokens > 0 {
		params["max_tokens"] = fmt.Sprint(*maxTokens)
	}
	body, _ := json.Marshal(map[string]any{"target": *target, "capability": "llm.generate", "params": params, "inputs": in})
	resp, err := c.http.Post(c.base+"/workloads", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var wl struct {
		ID     string `json:"id"`
		Target string `json:"target"`
		State  string `json:"state"`
	}
	json.Unmarshal(raw, &wl)
	if wl.State == "QUEUED" {
		fmt.Fprintf(os.Stderr, "(queued: every device with %s is busy)\n", *model)
	}
	return c.followWorkload(wl.ID)
}

// cmdModelTask downloads (llm.pull) or removes (llm.remove) a model on
// one device, showing its progress until it ends.
func (c *apiClient) cmdModelTask(capability, target, model string) error {
	body, _ := json.Marshal(map[string]any{"capability": capability, "target": target, "params": map[string]string{"model": model}})
	resp, err := c.http.Post(c.base+"/workloads", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var wl struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	json.Unmarshal(raw, &wl)
	if wl.State == "QUEUED" {
		var w workloadView
		if c.get("/workloads/"+wl.ID, &w) == nil && w.Waiting != "" {
			fmt.Fprintf(os.Stderr, "(waiting: %s)\n", w.Waiting)
		}
	}
	return c.followWorkload(wl.ID)
}

// followWorkload prints a workload's output as it arrives until it ends;
// Ctrl-C cancels it.
func (c *apiClient) followWorkload(id string) error {
	wl := struct{ ID string }{id}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	printed := 0
	for {
		select {
		case <-interrupt:
			c.http.Post(c.base+"/workloads/"+wl.ID+"/cancel", "application/json", nil)
			fmt.Fprintln(os.Stderr, "\n(canceled)")
			return nil
		case <-time.After(400 * time.Millisecond):
		}
		var w workloadView
		if err := c.get("/workloads/"+wl.ID, &w); err != nil {
			return err
		}
		if len(w.Stdout) > printed {
			fmt.Print(w.Stdout[printed:])
			printed = len(w.Stdout)
		}
		switch w.State {
		case "COMPLETED":
			fmt.Println()
			if w.Stderr != "" {
				fmt.Fprintf(os.Stderr, "(%s on %s)\n", strings.TrimSpace(w.Stderr), w.Target)
			}
			return nil
		case "FAILED", "CANCELED":
			fmt.Println()
			return fmt.Errorf("%s: %s", strings.ToLower(string(w.State)), w.Error)
		}
	}
}
