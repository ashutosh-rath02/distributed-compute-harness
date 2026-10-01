package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"home-harness/internal/domain"
)

// Batch jobs (internal/manager/jobs.go).

// cmdMap builds a job with one task per -each file (or -count copies)
// and submits it.
func cmdMap(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	name := fs.String("name", "", "a label for the job")
	attempts := fs.Int("attempts", 0, "attempts per task, the first run included (default 3)")
	count := fs.Int("count", 0, "run this many copies instead of one task per -each file; {i} in the args becomes the copy's number")
	target := fs.String("target", "", "pin every task to this node (default: spread over the fleet)")
	minMem := fs.String("min-mem", "", "minimum available memory each task needs, e.g. 1GiB")
	reduce := fs.String("reduce", "", `fan-in step run after every task succeeds, e.g. "python merge.py" (split on spaces); it gets each task's outputs at parts/<task>/<name>`)
	typ := fs.String("type", "", "run this typed task type per file (see harnessctl tasks); the arguments are then its key=value parameters")
	reduceType := fs.String("reduce-type", "", "typed fan-in step, e.g. archive.zip")
	var reduceParams fileList
	fs.Var(&reduceParams, "reduce-param", "key=value parameter of the -reduce-type step, repeatable")
	var each, shared, outs, reduceOuts, reduceShared fileList
	fs.Var(&each, "each", "input file or glob; one task per file, which lands in its working directory under its base name ({in} in the args), repeatable")
	fs.Var(&shared, "shared", "file every task gets (e.g. the script it runs), repeatable")
	fs.Var(&outs, "out", "output file each task must produce, repeatable")
	fs.Var(&reduceOuts, "reduce-out", "output file the reduce must produce (the job's result), repeatable")
	fs.Var(&reduceShared, "reduce-shared", "file the reduce gets besides the parts, repeatable")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harnessctl map [-each FILE|GLOB ... | -count N] [-shared FILE ...] [-out NAME ...] [-attempts N] [-reduce \"cmd args\" [-reduce-out NAME ...]] <cmd> [args, with {in} or {i}]")
	}
	// A raw command keeps everything after it as its own arguments (which
	// may look like flags); a typed job takes key=value parameters, so
	// flags may come after them too.
	typed := false
	for _, a := range args {
		typed = typed || a == "-type" || a == "--type" || strings.HasPrefix(a, "-type=") || strings.HasPrefix(a, "--type=")
	}
	var pos []string
	if typed {
		var err error
		if pos, err = parseTyped(fs, args); err != nil {
			return err
		}
	} else {
		if err := fs.Parse(args); err != nil {
			return err
		}
		pos = fs.Args()
	}
	if len(pos) < 1 && *typ == "" {
		fs.Usage()
		return errors.New("missing the command to run (or -type)")
	}
	var files []string
	for _, pattern := range each {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("-each %s: %w", pattern, err)
		}
		if len(matches) == 0 {
			matches = []string{pattern} // a plain path; the upload reports it if missing
		}
		files = append(files, matches...)
	}
	if (len(files) == 0) == (*count == 0) {
		return errors.New("give either -each files or -count N")
	}
	var req domain.ResourceRequirements
	if *minMem != "" {
		n, err := parseBytes(*minMem)
		if err != nil {
			return fmt.Errorf("-min-mem: %w", err)
		}
		req.MinMemoryBytes = n
	}
	sharedIn, err := c.resolveInputs(shared)
	if err != nil {
		return err
	}
	task := func(label string, inputs []map[string]string, replace func(string) string) map[string]any {
		if *typ != "" {
			params := map[string]string{}
			for _, kv := range pos {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue // reported below
				}
				params[k] = replace(v)
			}
			return map[string]any{
				"name": label, "target": *target, "capability": *typ, "params": params,
				"inputs": append(append([]map[string]string{}, sharedIn...), inputs...), "requirements": req,
			}
		}
		argv := make([]string, 0, len(pos)-1)
		for _, a := range pos[1:] {
			argv = append(argv, replace(a))
		}
		return map[string]any{
			"name": label, "target": *target, "command": pos[0], "args": argv,
			"inputs": append(append([]map[string]string{}, sharedIn...), inputs...), "outputs": outs, "requirements": req,
		}
	}
	var tasks []map[string]any
	if *count > 0 {
		for i := 0; i < *count; i++ {
			n := strconv.Itoa(i)
			tasks = append(tasks, task("#"+n, nil, func(a string) string { return strings.ReplaceAll(a, "{i}", n) }))
		}
	} else {
		for i, f := range files {
			base := filepath.Base(f)
			if err := domain.ValidArtifactName(base); err != nil {
				return fmt.Errorf("-each %s: %w (rename it)", f, err)
			}
			fmt.Fprintf(os.Stderr, "uploading %d/%d %s\n", i+1, len(files), f)
			info, err := c.uploadFile(f)
			if err != nil {
				return fmt.Errorf("-each %s: %w", f, err)
			}
			n := strconv.Itoa(i)
			tasks = append(tasks, task(base, []map[string]string{{"name": base, "sha256": info.SHA256}}, func(a string) string {
				return strings.ReplaceAll(strings.ReplaceAll(a, "{in}", base), "{i}", n)
			}))
		}
	}
	if *typ != "" {
		for _, kv := range pos {
			if !strings.Contains(kv, "=") {
				return fmt.Errorf("with -type, arguments are key=value parameters: %q isn't", kv)
			}
		}
	}
	body := map[string]any{"name": *name, "tasks": tasks, "maxAttempts": *attempts}
	if *reduceType != "" {
		params, err := parseParams(reduceParams)
		if err != nil {
			return err
		}
		rin, err := c.resolveInputs(reduceShared)
		if err != nil {
			return err
		}
		body["reduce"] = map[string]any{"capability": *reduceType, "params": params, "inputs": rin}
	} else if *reduce != "" {
		argv := strings.Fields(*reduce)
		rin, err := c.resolveInputs(reduceShared)
		if err != nil {
			return err
		}
		body["reduce"] = map[string]any{"command": argv[0], "args": argv[1:], "inputs": rin, "outputs": reduceOuts}
	}
	raw, _ := json.Marshal(body)
	resp, err := c.http.Post(c.base+"/jobs", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("submit job: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var job jobView
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return err
	}
	fmt.Printf("Job submitted: %s (%d tasks)\n", job.ID, len(tasks))
	fmt.Printf("Follow it with: harnessctl job %s\n", job.ID)
	return nil
}

type jobTaskView struct {
	Key      string               `json:"key"`
	Name     string               `json:"name"`
	State    string               `json:"state"`
	Attempts int                  `json:"attempts"`
	Workload string               `json:"workload"`
	Node     string               `json:"node"`
	NodeName string               `json:"nodeName"`
	Outputs  []domain.ArtifactRef `json:"outputs"`
	Error    string               `json:"error"`
	Waiting  string               `json:"waiting"`
}

type jobView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Error       string `json:"error"`
	MaxAttempts int    `json:"maxAttempts"`
	Counts      struct {
		Total, Completed, Active, Waiting, Failed, Canceled int
	} `json:"counts"`
	Tasks   []jobTaskView        `json:"tasks"`
	Reduce  *jobTaskView         `json:"reduce"`
	Outputs []domain.ArtifactRef `json:"outputs"`
}

func (c *apiClient) cmdJobs() error {
	var jobs []jobView
	if err := c.get("/jobs", &jobs); err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("No jobs yet.")
		return nil
	}
	fmt.Printf("%-34s %-10s %-9s %-7s %s\n", "JOB ID", "STATE", "DONE", "FAILED", "NAME")
	for _, j := range jobs {
		fmt.Printf("%-34s %-10s %-9s %-7d %s\n", j.ID, j.State, fmt.Sprintf("%d/%d", j.Counts.Completed, j.Counts.Total), j.Counts.Failed, j.Name)
	}
	return nil
}

func printTask(t jobTaskView) {
	detail := t.Error
	if t.Waiting != "" {
		detail = "waiting: " + t.Waiting
	}
	files := ""
	if len(t.Outputs) > 0 {
		files = fmt.Sprintf("%d file(s)", len(t.Outputs))
	}
	node := t.NodeName
	if node == "" {
		node = t.Node
	}
	fmt.Printf("  %-7s %-20s %-10s %-8d %-26s %-10s %s\n", t.Key, truncate(t.Name, 20), t.State, t.Attempts, truncate(node, 26), files, detail)
}

func (c *apiClient) cmdJob(id string) error {
	var j jobView
	if err := c.get("/jobs/"+id, &j); err != nil {
		return err
	}
	fmt.Printf("Job       %s %s\n", j.ID, j.Name)
	fmt.Printf("State     %s", j.State)
	if j.Error != "" {
		fmt.Printf(" (%s)", j.Error)
	}
	fmt.Printf("\nProgress  %d/%d completed, %d running, %d waiting, %d failed, %d canceled (up to %d attempts each)\n\n",
		j.Counts.Completed, j.Counts.Total, j.Counts.Active, j.Counts.Waiting, j.Counts.Failed, j.Counts.Canceled, j.MaxAttempts)
	fmt.Printf("  %-7s %-20s %-10s %-8s %-26s %-10s %s\n", "TASK", "NAME", "STATE", "ATTEMPTS", "NODE", "OUTPUTS", "")
	for _, t := range j.Tasks {
		printTask(t)
	}
	if j.Reduce != nil {
		printTask(*j.Reduce)
	}
	if len(j.Outputs) > 0 || hasTaskOutputs(j) {
		fmt.Printf("\nFetch the results with: harnessctl job-outputs %s [dir]\n", j.ID)
	}
	return nil
}

func hasTaskOutputs(j jobView) bool {
	for _, t := range j.Tasks {
		if len(t.Outputs) > 0 {
			return true
		}
	}
	return false
}

// cmdJobOutputs downloads a job's results into dir: the reduce's outputs
// at the top, each task's under parts/<task>/ (the layout the reduce saw).
func cmdJobOutputs(c *apiClient, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: harnessctl job-outputs <job-id> [dir]")
	}
	dir := "."
	if len(args) >= 2 {
		dir = args[1]
	}
	var j jobView
	if err := c.get("/jobs/"+args[0], &j); err != nil {
		return err
	}
	save := func(rel string, o domain.ArtifactRef) error {
		if err := domain.ValidArtifactName(o.Name); err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		n, err := c.download(o.SHA256, o.Name, dest)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		fmt.Printf("%s (%s)\n", dest, humanBytes(uint64(n)))
		return nil
	}
	saved := 0
	for _, o := range j.Outputs {
		if err := save(o.Name, o); err != nil {
			return err
		}
		saved++
	}
	for _, t := range j.Tasks {
		for _, o := range t.Outputs {
			if err := save(domain.ReducePartName(t.Key, o.Name), o); err != nil {
				return err
			}
			saved++
		}
	}
	if saved == 0 {
		return fmt.Errorf("job %s has no outputs yet (state %s)", j.ID, j.State)
	}
	return nil
}

func (c *apiClient) cmdJobCancel(id string) error {
	resp, err := c.http.Post(c.base+"/jobs/"+id+"/cancel", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Println("Job canceled; its running tasks are being stopped.")
	return nil
}
