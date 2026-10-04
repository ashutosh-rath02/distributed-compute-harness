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
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/tasks"
)

// Batch AI over a folder: summarise, classify or embed every file, one
// task per file spread over the devices that have the model, the results
// collected into one report (report.collect) and saved here. Each is a
// job, so "map -type llm.classify ... -reduce-type report.collect" does
// the same by hand.

const defaultSummaryPrompt = "Summarise the document above in two or three sentences. Write only the summary."

// batchOptions are the flags every batch command takes.
type batchOptions struct {
	model, out, dir, name *string
	attempts              *int
}

func addBatchFlags(fs *flag.FlagSet, report string) batchOptions {
	return batchOptions{
		model:    fs.String("model", "", "model to use (default: the only one the fleet has for this; see harnessctl tasks)"),
		out:      fs.String("out", report, "report file name: .md, .csv or .json"),
		dir:      fs.String("dir", ".", "folder to save the report in"),
		name:     fs.String("name", "", "a label for the job"),
		attempts: fs.Int("attempts", 0, "attempts per file, the first run included (default 3)"),
	}
}

func cmdSummarize(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	prompt := fs.String("prompt", defaultSummaryPrompt, "what to ask about each file (it comes after the file's text)")
	maxTokens := fs.Int("max-tokens", 0, "longest answer per file, in tokens (default 512)")
	o := addBatchFlags(fs, "summaries.md")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: harnessctl summarize [-model M] [-prompt "..."] [-out summaries.md] FILE|GLOB ...`)
	}
	patterns, err := parseTyped(fs, args)
	if err != nil {
		return err
	}
	params := map[string]string{"prompt": *prompt}
	if *maxTokens > 0 {
		params["max_tokens"] = strconv.Itoa(*maxTokens)
	}
	// llm.generate frames each file as "--- name ---\n<text>\n\n" within
	// its context limit.
	framing := func(name string) int { return len("--- "+name+" ---\n") + 2 }
	return c.runAIBatch(fs, o, "llm.generate", params, patterns, framing)
}

func cmdClassify(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("classify", flag.ContinueOnError)
	labels := fs.String("labels", "", "the labels to choose from, separated by commas, e.g. invoice,receipt,letter (required)")
	o := addBatchFlags(fs, "labels.csv")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: harnessctl classify -labels a,b,c [-model M] [-out labels.csv] FILE|GLOB ...`)
	}
	patterns, err := parseTyped(fs, args)
	if err != nil {
		return err
	}
	if *labels == "" {
		fs.Usage()
		return errors.New("missing -labels")
	}
	if _, err := catalog.ParseLabels(*labels); err != nil {
		return fmt.Errorf("-labels: %w", err)
	}
	return c.runAIBatch(fs, o, "llm.classify", map[string]string{"labels": *labels}, patterns, nil)
}

func cmdEmbed(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("embed", flag.ContinueOnError)
	o := addBatchFlags(fs, "embeddings.json")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: harnessctl embed [-model M] [-out embeddings.json] FILE|GLOB ...`)
	}
	patterns, err := parseTyped(fs, args)
	if err != nil {
		return err
	}
	return c.runAIBatch(fs, o, "llm.embed", map[string]string{}, patterns, nil)
}

// runAIBatch runs capability once per file as a job with a report.collect
// step, follows it, and saves the report.
func (c *apiClient) runAIBatch(fs *flag.FlagSet, o batchOptions, capability domain.CapabilityName, params map[string]string, patterns []string, framing func(string) int) error {
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("%s: %w", pattern, err)
		}
		if len(matches) == 0 {
			matches = []string{pattern} // a plain path; checked below
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		fs.Usage()
		return errors.New("give the files, e.g. docs/*.txt")
	}
	// A file the model can't take would fail its task on every attempt —
	// and with it the whole job and its report — so it is left out here,
	// saying why.
	var use []string
	for _, f := range files {
		if why := unfitForModel(f, framing); why != "" {
			fmt.Fprintf(os.Stderr, "skipping %s: %s\n", f, why)
			continue
		}
		use = append(use, f)
	}
	switch {
	case len(use) == 0:
		return errors.New("no file left to run")
	case len(use) > domain.MaxWorkloadInputs-1:
		return fmt.Errorf("%d files: at most %d at once (the report step takes at most %d results); split the folder", len(use), domain.MaxWorkloadInputs-1, domain.MaxWorkloadInputs-1)
	}
	if *o.model == "" {
		model, err := c.defaultModel(capability)
		if err != nil {
			return err
		}
		*o.model = model
	}
	params["model"] = *o.model
	var taskList []map[string]any
	for i, f := range use {
		base := filepath.Base(f)
		fmt.Fprintf(os.Stderr, "uploading %d/%d %s\n", i+1, len(use), f)
		info, err := c.uploadFile(f)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		taskList = append(taskList, map[string]any{
			"name": base, "capability": capability, "params": params,
			"inputs": []map[string]string{{"name": base, "sha256": info.SHA256}},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"name": *o.name, "tasks": taskList, "maxAttempts": *o.attempts,
		"reduce": map[string]any{"capability": "report.collect", "params": map[string]string{"name": *o.out}},
	})
	resp, err := c.http.Post(c.base+"/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("submit job: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var job jobView
	if err := json.Unmarshal(raw, &job); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Job %s: %d file(s) with %s, spread over the devices that have it (Ctrl-C stops watching; the job keeps running)\n", job.ID, len(use), *o.model)
	return c.followAIBatch(job.ID, filepath.Join(*o.dir, *o.out))
}

// unfitForModel says why a model can't take file f ("" if it can): a
// local model takes at most tasks.MaxContextBytes of UTF-8 text.
func unfitForModel(f string, framing func(string) int) string {
	if err := domain.ValidArtifactName(filepath.Base(f)); err != nil {
		return err.Error() + " (rename it)"
	}
	st, err := os.Stat(f)
	if err != nil {
		return err.Error()
	}
	limit := int64(tasks.MaxContextBytes)
	if framing != nil {
		limit -= int64(framing(filepath.Base(f)))
	}
	switch {
	case st.IsDir():
		return "a folder (give its files, e.g. folder/*.txt)"
	case st.Size() > limit:
		return fmt.Sprintf("over %d KiB, more than a local model takes in at once", tasks.MaxContextBytes>>10)
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return err.Error()
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	switch {
	case !utf8.Valid(data):
		return "not UTF-8 text"
	case len(bytes.TrimSpace(data)) == 0:
		return "empty"
	}
	return ""
}

// defaultModel is the model to use for capability when none was given:
// one the fleet offers for it (the catalog's choices — for llm.embed only
// embedding models).
func (c *apiClient) defaultModel(capability domain.CapabilityName) (string, error) {
	var cat struct {
		Types []struct {
			Name    domain.CapabilityName `json:"name"`
			Choices map[string][]string   `json:"choices"`
		} `json:"types"`
	}
	if err := c.get("/catalog", &cat); err != nil {
		return "", err
	}
	var models []string
	for _, t := range cat.Types {
		if t.Name == capability {
			models = t.Choices["model"]
		}
	}
	switch {
	case len(models) == 0 && capability == "llm.embed":
		return "", errors.New("no device has an embedding model yet (on one with Ollama: ollama pull embeddinggemma)")
	case len(models) == 0:
		return "", fmt.Errorf("no device offers %s with a model yet (see harnessctl models)", capability)
	case len(models) > 1:
		fmt.Fprintf(os.Stderr, "(using %s; the fleet also has %s — pick one with -model)\n", models[0], strings.Join(models[1:], ", "))
	}
	return models[0], nil
}

// followAIBatch prints a job's progress until it ends, then saves its
// report to dest (and shows a short one). Ctrl-C stops watching only.
func (c *apiClient) followAIBatch(id, dest string) error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	last := ""
	var j jobView
	for {
		j = jobView{} // a field the next answer leaves out mustn't keep this one's value
		if err := c.get("/jobs/"+id, &j); err != nil {
			return err
		}
		line := fmt.Sprintf("%d/%d done", j.Counts.Completed, j.Counts.Total)
		if j.Counts.Active > 0 { // running or queued for a device
			line += fmt.Sprintf(", %d in progress", j.Counts.Active)
		}
		if j.Counts.Waiting > 0 {
			line += fmt.Sprintf(", %d waiting", j.Counts.Waiting)
		}
		if j.Counts.Failed > 0 {
			line += fmt.Sprintf(", %d failed", j.Counts.Failed)
		}
		if j.Counts.Completed == j.Counts.Total && j.Reduce != nil && j.State == "RUNNING" {
			line += ", writing the report"
		}
		if line != last {
			fmt.Fprintln(os.Stderr, line)
			last = line
		}
		if j.State != "RUNNING" {
			break
		}
		select {
		case <-interrupt:
			fmt.Fprintf(os.Stderr, "(stopped watching; the job keeps running: harnessctl job %s, or harnessctl job-cancel %s to stop it)\n", id, id)
			return nil
		case <-time.After(time.Second):
		}
	}
	if j.State != "COMPLETED" || len(j.Outputs) == 0 {
		for _, t := range append(j.Tasks, derefTask(j.Reduce)...) {
			if name := t.Name; t.Error != "" {
				if name == "" {
					name = t.Key
				}
				fmt.Fprintf(os.Stderr, "  %s: %s\n", name, t.Error)
			}
		}
		return fmt.Errorf("the job %s: %s (what finished: harnessctl job-outputs %s)", strings.ToLower(j.State), j.Error, id)
	}
	report := j.Outputs[0]
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	n, err := c.download(report.SHA256, report.Name, dest)
	if err != nil {
		return err
	}
	if ext := filepath.Ext(dest); n <= 16<<10 && (ext == ".md" || ext == ".csv") {
		if data, err := os.ReadFile(dest); err == nil {
			fmt.Println(strings.TrimRight(string(data), "\n"))
		}
	}
	fmt.Fprintf(os.Stderr, "Saved %s (%s)\n", dest, humanBytes(uint64(n)))
	return nil
}

func derefTask(t *jobTaskView) []jobTaskView {
	if t == nil {
		return nil
	}
	return []jobTaskView{*t}
}
