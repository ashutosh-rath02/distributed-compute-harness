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
	"strings"
)

// harnessctl mapreduce: one big text file cut into chunks (text.split), a
// task run on every chunk across the fleet (the map), and the chunks'
// results folded into one (the reduce) — built as a two-step workflow:
// step 1 splits, step 2 runs the map once per chunk and combines.

const mapReduceUsage = `usage: harnessctl mapreduce -in BIG.txt -chunks N -map "TYPE [key=value ...]" -reduce "TYPE [key=value ...]" [-by lines|bytes] [-attempts N] [-name N]
example (word count): harnessctl mapreduce -in big.txt -chunks 8 -map text.count -reduce "json.merge mode=sum field=total"`

func buildMapReduce(name string, chunks int, by, mapSpec, reduceSpec string, attempts int, inputs []map[string]string) (map[string]any, error) {
	if chunks < 1 || chunks > 16 {
		return nil, errors.New("-chunks must be 1-16 (a task can write at most 16 files)")
	}
	if by != "lines" && by != "bytes" {
		return nil, errors.New("-by must be lines or bytes")
	}
	mapStep, err := parseStep(mapSpec, false)
	if err != nil {
		return nil, fmt.Errorf("-map: %w", err)
	}
	reduceStep, err := parseStep(reduceSpec, false)
	if err != nil {
		return nil, fmt.Errorf("-reduce: %w", err)
	}
	mapStep["mode"], mapStep["combine"] = "perFile", reduceStep
	stages := []any{
		map[string]any{"type": "text.split", "mode": "once", "params": map[string]string{"chunks": fmt.Sprint(chunks), "by": by}},
		mapStep,
	}
	for _, st := range stages {
		stringParams(st)
	}
	body := map[string]any{"stages": stages, "inputs": inputs}
	if name != "" {
		body["name"] = name
	}
	if attempts != 0 {
		body["maxAttempts"] = attempts
	}
	return body, nil
}

func cmdMapReduce(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("mapreduce", flag.ContinueOnError)
	in := fs.String("in", "", "the big text file")
	chunks := fs.Int("chunks", 4, "how many chunks to cut it into (1-16)")
	by := fs.String("by", "lines", "cut by lines (equal line counts) or bytes (about equal sizes); always at line ends")
	mapSpec := fs.String("map", "", `the task run on every chunk: "TYPE [key=value ...]", e.g. text.count`)
	reduceSpec := fs.String("reduce", "", `the task that folds the chunks' results into one: "TYPE [key=value ...]", e.g. "json.merge mode=sum field=total"`)
	attempts := fs.Int("attempts", 0, "attempts per task, the first run included (default 3)")
	name := fs.String("name", "", "a label for the workflow")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, mapReduceUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *mapSpec == "" || *reduceSpec == "" || fs.NArg() != 0 {
		fs.Usage()
		return errors.New("give -in, -map and -reduce")
	}
	uploaded, err := c.resolveInputs([]string{*in})
	if err != nil {
		return err
	}
	if len(uploaded) != 1 {
		return fmt.Errorf("-in %s: expected one file", *in)
	}
	body, err := buildMapReduce(*name, *chunks, *by, *mapSpec, *reduceSpec, *attempts, nil)
	if err != nil {
		return err
	}
	body["inputs"] = uploaded
	raw, _ := json.Marshal(body)
	resp, err := c.http.Post(c.base+"/workflows", "application/json", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("submit workflow: %w", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(answer)))
	}
	var wf workflowView
	if err := json.Unmarshal(answer, &wf); err != nil {
		return err
	}
	fmt.Printf("Map-reduce submitted: %s (split into %d chunks, then %q on each, then %q)\n", wf.ID, *chunks, *mapSpec, *reduceSpec)
	fmt.Printf("Follow it with: harnessctl workflow show %s\nFetch the result with: harnessctl workflow outputs %s [dir]\n", wf.ID, wf.ID)
	return nil
}
