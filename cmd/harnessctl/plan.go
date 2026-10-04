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
	"sort"
	"strings"
	"time"
)

// The AI planner (the manager's planner.go): a request in plain words
// becomes a checked plan, run only once approved.

type planStepView struct {
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	Params    map[string]string `json:"params"`
	Effective map[string]string `json:"effective"`
}

type planView struct {
	ID          string        `json:"id"`
	State       string        `json:"state"`
	Request     string        `json:"request"`
	Model       string        `json:"model"`
	ChatState   string        `json:"chatState"`
	Summary     string        `json:"summary"`
	ModelReason string        `json:"modelReason"`
	Problem     string        `json:"problem"`
	Task        *planStepView `json:"task"`
	Mode        string        `json:"mode"`
	Parts       int           `json:"parts"`
	UseFiles    []string      `json:"useFiles"`
	Combine     *planStepView `json:"combine"`
	Tasks       int           `json:"tasks"`
	JobID       string        `json:"jobId"`
	CreatedAt   time.Time     `json:"createdAt"`
	ExpiresAt   time.Time     `json:"expiresAt"`
	// Follow-up steps: such a plan runs as a workflow.
	Then       []planThenView `json:"then"`
	WorkflowID string         `json:"workflowId"`
}

type planThenView struct {
	planStepView
	Mode string `json:"mode"`
	Runs int    `json:"runs"`
}

type planList struct {
	Plans  []planView `json:"plans"`
	Models []string   `json:"models"`
}

const planUsage = `usage: harnessctl plan [-model M] [-in FILE ...] "what to do"
       harnessctl plan show|approve|reject <plan-id>`

func cmdPlan(c *apiClient, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "show", "approve", "reject":
			if len(args) != 2 {
				return errors.New(planUsage)
			}
			return c.planAction(args[0], args[1])
		}
	}
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	model := fs.String("model", "", "the model that plans (default: the only one that can)")
	var inputs fileList
	fs.Var(&inputs, "in", "a file the job is about, repeatable")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, planUsage) }
	pos, err := parseTyped(fs, args)
	if err != nil {
		return err
	}
	request := strings.TrimSpace(strings.Join(pos, " "))
	if request == "" {
		fs.Usage()
		return errors.New("say what you want done")
	}
	if *model == "" {
		var list planList
		if err := c.get("/plans", &list); err != nil {
			return err
		}
		switch len(list.Models) {
		case 0:
			return errors.New("no device can plan yet: it needs a local model and an updated agent (see harnessctl models)")
		case 1:
		default:
			fmt.Fprintf(os.Stderr, "(planning with %s; also possible: %s — pick one with -model)\n", list.Models[0], strings.Join(list.Models[1:], ", "))
		}
		*model = list.Models[0]
	}
	files, err := c.resolveInputs(inputs)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"request": request, "model": *model, "files": files})
	resp, err := c.http.Post(c.base+"/plans", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var p planView
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	fmt.Printf("Asking %s to plan it", p.Model)
	waiting := false
	for p.State == "planning" {
		time.Sleep(time.Second)
		if err := c.get("/plans/"+p.ID, &p); err != nil {
			return err
		}
		if p.ChatState == "QUEUED" && !waiting {
			fmt.Print(" (waiting for the model's device)")
			waiting = true
		}
		fmt.Print(".")
	}
	fmt.Println()
	printPlan(p)
	return nil
}

func (c *apiClient) planAction(action, id string) error {
	var p planView
	if action == "show" {
		if err := c.get("/plans/"+id, &p); err != nil {
			return err
		}
		printPlan(p)
		return nil
	}
	resp, err := c.http.Post(c.base+"/plans/"+id+"/"+action, "application/json", nil)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if action == "approve" && p.WorkflowID != "" {
		fmt.Printf("Approved: running as workflow %s\n  follow it: harnessctl workflow show %s\n  results:   harnessctl workflow outputs %s\n", p.WorkflowID, p.WorkflowID, p.WorkflowID)
		return nil
	}
	if action == "approve" {
		fmt.Printf("Approved: running as job %s\n  follow it: harnessctl job %s\n  results:   harnessctl job-outputs %s\n", p.JobID, p.JobID, p.JobID)
		return nil
	}
	fmt.Println("Rejected.")
	return nil
}

func (c *apiClient) cmdPlans() error {
	var list planList
	if err := c.get("/plans", &list); err != nil {
		return err
	}
	if len(list.Plans) == 0 {
		fmt.Println("No plans yet: harnessctl plan \"what to do\"")
	}
	for _, p := range list.Plans {
		what := p.Request
		if len(what) > 60 {
			what = what[:57] + "..."
		}
		fmt.Printf("%-16s  %-9s  %-12s  %s\n", p.ID, p.State, p.Model, what)
	}
	if len(list.Models) > 0 {
		fmt.Printf("\nModels that can plan: %s\n", strings.Join(list.Models, ", "))
	}
	return nil
}

func printStep(n int, label string, s *planStepView) {
	fmt.Printf("  %d. %s%s (%s)\n", n, label, s.Title, s.Type)
	params := s.Effective
	if len(params) == 0 {
		params = s.Params
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := params[k]
		if len(v) > 100 {
			v = v[:97] + "..."
		}
		fmt.Printf("       %s = %s\n", k, v)
	}
}

func printPlan(p planView) {
	fmt.Printf("Plan %s: %s (model %s)\n", p.ID, p.State, p.Model)
	fmt.Printf("  you asked: %q\n", p.Request)
	if p.Task != nil {
		printStep(1, "", p.Task)
		switch p.Mode {
		case "perFile":
			fmt.Printf("       once per file, %d runs spread over the devices: %s\n", p.Tasks, strings.Join(p.UseFiles, ", "))
		case "parts":
			fmt.Printf("       in %d parts, spread over the devices\n", p.Parts)
		default:
			if len(p.UseFiles) > 0 {
				fmt.Printf("       once, with %s\n", strings.Join(p.UseFiles, ", "))
			} else {
				fmt.Println("       once")
			}
		}
		n := 1
		if p.Combine != nil {
			n++
			printStep(n, "then ", p.Combine)
		}
		for _, t := range p.Then {
			n++
			printStep(n, "then ", &t.planStepView)
			if t.Mode == "perFile" {
				fmt.Printf("       once per file the step before made (%d runs)\n", t.Runs)
			} else {
				fmt.Println("       once, with every file the step before made")
			}
		}
	}
	if p.Summary != "" {
		fmt.Printf("  the model says: %q\n", p.Summary)
	}
	if p.Problem != "" {
		fmt.Printf("  refused: %s\n", p.Problem)
	}
	if p.ModelReason != "" && p.State == "refused" {
		fmt.Printf("  the model's reason: %q\n", p.ModelReason)
	}
	switch p.State {
	case "proposed":
		fmt.Printf("\nNothing has run. Approve within an hour to run it:\n  harnessctl plan approve %s\n  harnessctl plan reject %s\n", p.ID, p.ID)
	case "approved":
		if p.WorkflowID != "" {
			fmt.Printf("  running as workflow %s (harnessctl workflow show %s)\n", p.WorkflowID, p.WorkflowID)
		} else {
			fmt.Printf("  running as job %s (harnessctl job %s)\n", p.JobID, p.JobID)
		}
	}
}
