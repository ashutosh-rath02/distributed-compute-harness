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
	"path/filepath"
	"strconv"
	"strings"

	"home-harness/internal/domain"
)

// Workflows (the manager's workflows.go): steps run in order, each on the
// results of the step before.

const workflowUsage = `usage: harnessctl workflow [-name N] [-attempts N] [-in FILE|GLOB ...] FILE.json
       harnessctl workflow [-name N] [-attempts N] [-in FILE|GLOB ...] -step "TYPE [once|perFile|parts=N] [key=value ...]" [-combine "TYPE [key=value ...]"] -step ...
       harnessctl workflow show|cancel <workflow-id>
       harnessctl workflow outputs <workflow-id> [dir]`

// stepFlags collects -step and -combine in command-line order: a
// -combine belongs to the -step before it.
type stepFlags struct{ stages []any }

type stepFlag struct{ s *stepFlags }

func (f stepFlag) String() string { return "" }
func (f stepFlag) Set(v string) error {
	step, err := parseStep(v, true)
	if err != nil {
		return err
	}
	f.s.stages = append(f.s.stages, step)
	return nil
}

type combineFlag struct{ s *stepFlags }

func (f combineFlag) String() string { return "" }
func (f combineFlag) Set(v string) error {
	if len(f.s.stages) == 0 {
		return errors.New("-combine goes after the -step whose results it combines")
	}
	step, err := parseStep(v, false)
	if err != nil {
		return err
	}
	f.s.stages[len(f.s.stages)-1].(map[string]any)["combine"] = step
	return nil
}

// parseStep reads "TYPE [once|perFile|parts=N] [key=value ...]" (the
// mode only for a step, not a combine).
func parseStep(v string, withMode bool) (map[string]any, error) {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return nil, errors.New(`empty step: give its task type, e.g. -step "archive.zip name=photos.zip"`)
	}
	step := map[string]any{"type": fields[0]}
	params := map[string]string{}
	for _, f := range fields[1:] {
		switch {
		case withMode && (f == "once" || f == "perFile" || f == "each"):
			step["mode"] = map[string]string{"once": "once", "perFile": "perFile", "each": "perFile"}[f]
		case withMode && strings.HasPrefix(f, "parts="):
			n, err := strconv.Atoi(strings.TrimPrefix(f, "parts="))
			if err != nil {
				return nil, fmt.Errorf("%q: parts must be a whole number", f)
			}
			step["mode"], step["parts"] = "parts", n
		default:
			k, val, ok := strings.Cut(f, "=")
			if !ok {
				if withMode {
					return nil, fmt.Errorf("%q: expected key=value, once, perFile or parts=N", f)
				}
				return nil, fmt.Errorf("%q: expected key=value", f)
			}
			params[k] = val
		}
	}
	if len(params) > 0 {
		step["params"] = params
	}
	return step, nil
}

// stringParams turns numbers and true/false in a hand-written workflow's
// params into the strings the API takes ({"width": 800} → "800").
func stringParams(step any) {
	m, ok := step.(map[string]any)
	if !ok {
		return
	}
	if params, ok := m["params"].(map[string]any); ok {
		for k, v := range params {
			switch x := v.(type) {
			case float64:
				params[k] = strconv.FormatFloat(x, 'f', -1, 64)
			case bool:
				params[k] = strconv.FormatBool(x)
			}
		}
	}
	stringParams(m["combine"])
}

// expandFiles expands globs among -in values (a name=... value is taken
// as it is).
func expandFiles(specs []string) ([]string, error) {
	var out []string
	for _, spec := range specs {
		if strings.Contains(spec, "=") {
			out = append(out, spec)
			continue
		}
		matches, err := filepath.Glob(spec)
		if err != nil {
			return nil, fmt.Errorf("-in %s: %w", spec, err)
		}
		if len(matches) == 0 {
			matches = []string{spec} // a plain path; the upload reports it if missing
		}
		out = append(out, matches...)
	}
	return out, nil
}

func cmdWorkflow(c *apiClient, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "show", "cancel":
			if len(args) != 2 {
				return errors.New(workflowUsage)
			}
			if args[0] == "show" {
				return c.cmdWorkflowShow(args[1])
			}
			return c.cmdWorkflowCancel(args[1])
		case "outputs":
			return cmdWorkflowOutputs(c, args[1:])
		}
	}
	fs := flag.NewFlagSet("workflow", flag.ContinueOnError)
	name := fs.String("name", "", "a label for the workflow")
	attempts := fs.Int("attempts", 0, "attempts per task, the first run included (default 3)")
	var files fileList
	fs.Var(&files, "in", "a file (or glob) the first step works on, repeatable; name=sha256:HEX names one already stored")
	var steps stepFlags
	fs.Var(stepFlag{&steps}, "step", `a step, in order: "TYPE [once|perFile|parts=N] [key=value ...]" (once: one run with every file, the default; perFile: one run per file; parts=N: N parts, for types that split, like render.fractal)`)
	fs.Var(combineFlag{&steps}, "combine", `joins the results of the -step before it into one file: "TYPE [key=value ...]", e.g. "archive.zip name=all.zip"`)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, workflowUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	body := map[string]any{}
	switch {
	case len(pos) == 1 && len(steps.stages) == 0:
		raw, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("%s: %w", pos[0], err)
		}
		// "files" in the file: local paths, uploaded like -in.
		if fl, ok := body["files"].([]any); ok {
			for _, f := range fl {
				s, ok := f.(string)
				if !ok {
					return fmt.Errorf("%s: files are paths (strings)", pos[0])
				}
				files = append(files, s)
			}
		}
		delete(body, "files")
	case len(pos) == 0 && len(steps.stages) > 0:
		body["stages"] = steps.stages
	default:
		fs.Usage()
		return errors.New("give the steps as -step flags or in a JSON file, not both")
	}
	if *name != "" {
		body["name"] = *name
	}
	if *attempts != 0 {
		body["maxAttempts"] = *attempts
	}
	stages, _ := body["stages"].([]any)
	for _, st := range stages {
		stringParams(st)
	}
	expanded, err := expandFiles(files)
	if err != nil {
		return err
	}
	uploaded, err := c.resolveInputs(expanded)
	if err != nil {
		return err
	}
	inputs, _ := body["inputs"].([]any)
	for _, in := range uploaded {
		inputs = append(inputs, in)
	}
	if len(inputs) > 0 {
		body["inputs"] = inputs
	}
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
	fmt.Printf("Workflow submitted: %s (%d steps, %d files)\n", wf.ID, len(wf.Stages), len(wf.Inputs))
	fmt.Printf("Follow it with: harnessctl workflow show %s\n", wf.ID)
	return nil
}

type workflowStepView struct {
	Type  string `json:"type"`
	Title string `json:"title"`
}

type workflowStageView struct {
	Stage   int               `json:"stage"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Title   string            `json:"title"`
	Mode    string            `json:"mode"`
	Parts   int               `json:"parts"`
	Combine *workflowStepView `json:"combine"`
	State   string            `json:"state"`
	Job     string            `json:"job"`
	Counts  *struct {
		Total, Completed, Active, Waiting, Failed, Canceled int
	} `json:"counts"`
	Error   string               `json:"error"`
	Waiting string               `json:"waiting"`
	Outputs []domain.ArtifactRef `json:"outputs"`
}

type workflowView struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	State   string               `json:"state"`
	Error   string               `json:"error"`
	Stage   int                  `json:"stage"`
	Inputs  []domain.ArtifactRef `json:"inputs"`
	Stages  []workflowStageView  `json:"stages"`
	Outputs []domain.ArtifactRef `json:"outputs"`
}

func (c *apiClient) cmdWorkflows() error {
	var wfs []workflowView
	if err := c.get("/workflows", &wfs); err != nil {
		return err
	}
	if len(wfs) == 0 {
		fmt.Println("No workflows yet.")
		return nil
	}
	fmt.Printf("%-34s %-10s %-6s %s\n", "WORKFLOW ID", "STATE", "STEP", "NAME")
	for _, wf := range wfs {
		fmt.Printf("%-34s %-10s %-6s %s\n", wf.ID, wf.State, fmt.Sprintf("%d/%d", wf.Stage, len(wf.Stages)), wf.Name)
	}
	return nil
}

func stageHow(st workflowStageView) string {
	switch st.Mode {
	case "perFile":
		return "once per file"
	case "parts":
		return fmt.Sprintf("in %d parts", st.Parts)
	}
	return "once"
}

func (c *apiClient) cmdWorkflowShow(id string) error {
	var wf workflowView
	if err := c.get("/workflows/"+id, &wf); err != nil {
		return err
	}
	fmt.Printf("Workflow  %s %s\n", wf.ID, wf.Name)
	fmt.Printf("State     %s, step %d of %d", wf.State, wf.Stage, len(wf.Stages))
	if wf.Error != "" {
		fmt.Printf(" (%s)", wf.Error)
	}
	fmt.Printf("\nFiles     %d\n\n", len(wf.Inputs))
	fmt.Printf("  %-5s %-28s %-14s %-10s %-6s %s\n", "STEP", "WHAT", "HOW", "STATE", "DONE", "JOB")
	for _, st := range wf.Stages {
		done := ""
		if st.Counts != nil {
			done = fmt.Sprintf("%d/%d", st.Counts.Completed, st.Counts.Total)
		}
		what := st.Title
		if st.Name != "" {
			what = st.Name
		}
		fmt.Printf("  %-5d %-28s %-14s %-10s %-6s %s\n", st.Stage, truncate(what, 28), stageHow(st), st.State, done, st.Job)
		if st.Combine != nil {
			fmt.Printf("        then combined: %s (%s)\n", st.Combine.Title, st.Combine.Type)
		}
		if st.Waiting != "" {
			fmt.Printf("        waiting: %s\n", st.Waiting)
		}
		if st.Error != "" {
			fmt.Printf("        %s\n", st.Error)
		}
	}
	if len(wf.Outputs) > 0 {
		fmt.Println("\nResults:")
		for _, o := range wf.Outputs {
			fmt.Printf("  %s (%s)\n", o.Name, humanBytes(uint64(o.Size)))
		}
		fmt.Printf("Fetch them with: harnessctl workflow outputs %s [dir]\n", wf.ID)
	}
	return nil
}

func (c *apiClient) cmdWorkflowCancel(id string) error {
	resp, err := c.http.Post(c.base+"/workflows/"+id+"/cancel", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Println("Workflow canceled; its running step is being stopped.")
	return nil
}

// cmdWorkflowOutputs downloads a completed workflow's results into dir.
func cmdWorkflowOutputs(c *apiClient, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: harnessctl workflow outputs <workflow-id> [dir]")
	}
	dir := "."
	if len(args) >= 2 {
		dir = args[1]
	}
	var wf workflowView
	if err := c.get("/workflows/"+args[0], &wf); err != nil {
		return err
	}
	if len(wf.Outputs) == 0 {
		return fmt.Errorf("workflow %s has no results yet (state %s)", wf.ID, wf.State)
	}
	for _, o := range wf.Outputs {
		if err := domain.ValidArtifactName(o.Name); err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(o.Name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		n, err := c.download(o.SHA256, o.Name, dest)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Name, err)
		}
		fmt.Printf("%s (%s)\n", dest, humanBytes(uint64(n)))
	}
	return nil
}
