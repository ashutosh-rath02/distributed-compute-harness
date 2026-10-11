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
	"time"
)

// Schedules (the manager's schedules.go): a job or workflow run on a cron
// expression, an @macro or "every 15m".

const scheduleUsage = `usage: harnessctl schedule add -cron "EXPR" [-tz ZONE] [-name N] [-missed skip|run-once] [-overlap skip|queue|allow]
                              [-max-runs N] [-end 2027-01-01T00:00:00Z] (-job FILE.json | -workflow FILE.json [-in FILE ...])
       harnessctl schedule list
       harnessctl schedule show|pause|resume|run-now|delete <schedule-id>
EXPR: five cron fields ("0 3 * * *"), @hourly @daily @weekly @monthly @yearly, or "every 15m" / "every 2h" / "every 1d"`

type scheduleRunView struct {
	Slot     string    `json:"slot"`
	At       time.Time `json:"at"`
	Outcome  string    `json:"outcome"`
	Reason   string    `json:"reason"`
	Manual   bool      `json:"manual"`
	Job      string    `json:"job"`
	Workflow string    `json:"workflow"`
	State    string    `json:"state"`
}

type scheduleView struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Schedule string            `json:"schedule"`
	Timezone string            `json:"timezone"`
	Kind     string            `json:"kind"`
	Summary  string            `json:"summary"`
	State    string            `json:"state"`
	Missed   string            `json:"missed"`
	Overlap  string            `json:"overlap"`
	MaxRuns  int               `json:"maxRuns"`
	EndAt    *time.Time        `json:"endAt"`
	Runs     int               `json:"runs"`
	NextRun  *time.Time        `json:"nextRun"`
	LastRun  *scheduleRunView  `json:"lastRun"`
	History  []scheduleRunView `json:"history"`
	QueuedAt *time.Time        `json:"queuedSlot"`
}

func cmdSchedule(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New(scheduleUsage)
	}
	switch args[0] {
	case "add":
		return c.cmdScheduleAdd(args[1:])
	case "list":
		return c.cmdScheduleList()
	case "show":
		return requireArgs(args, 2, "schedule show <schedule-id>", func() error { return c.cmdScheduleShow(args[1]) })
	case "pause", "resume":
		return requireArgs(args, 2, "schedule "+args[0]+" <schedule-id>", func() error {
			return c.scheduleAction(args[1], args[0], "Schedule "+args[0]+"d.")
		})
	case "run-now":
		return requireArgs(args, 2, "schedule run-now <schedule-id>", func() error { return c.cmdScheduleRunNow(args[1]) })
	case "delete":
		return requireArgs(args, 2, "schedule delete <schedule-id>", func() error { return c.cmdScheduleDelete(args[1]) })
	}
	return errors.New(scheduleUsage)
}

func (c *apiClient) cmdScheduleAdd(args []string) error {
	fs := flag.NewFlagSet("schedule add", flag.ContinueOnError)
	cron := fs.String("cron", "", `when to run: five cron fields, an @macro, or "every 15m"`)
	tz := fs.String("tz", "UTC", "IANA time zone the cron fields are read in, e.g. Europe/Berlin")
	name := fs.String("name", "", "a label")
	missed := fs.String("missed", "skip", "slots that passed while the manager was off: skip or run-once")
	overlap := fs.String("overlap", "skip", "a slot that arrives while the last run is going: skip, queue or allow")
	maxRuns := fs.Int("max-runs", 0, "stop after this many runs (0 = no limit)")
	end := fs.String("end", "", "stop at this time (RFC 3339, e.g. 2027-01-01T00:00:00Z)")
	jobFile := fs.String("job", "", "a job to run, as for POST /jobs (JSON file)")
	wfFile := fs.String("workflow", "", "a workflow to run, as for `harnessctl workflow FILE.json` (JSON file; its files are uploaded now and kept for the schedule)")
	var files fileList
	fs.Var(&files, "in", "with -workflow: a file (or glob) the first step works on, repeatable")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, scheduleUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cron == "" || (*jobFile == "") == (*wfFile == "") {
		fs.Usage()
		return errors.New("give -cron and exactly one of -job or -workflow")
	}
	body := map[string]any{"schedule": *cron, "timezone": *tz, "missed": *missed, "overlap": *overlap}
	if *name != "" {
		body["name"] = *name
	}
	if *maxRuns != 0 {
		body["maxRuns"] = *maxRuns
	}
	if *end != "" {
		if _, err := time.Parse(time.RFC3339, *end); err != nil {
			return fmt.Errorf("-end: %w (like 2027-01-01T00:00:00Z)", err)
		}
		body["endAt"] = *end
	}
	path, key := *jobFile, "job"
	if *wfFile != "" {
		path, key = *wfFile, "workflow"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var work map[string]any
	if err := json.Unmarshal(raw, &work); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if key == "workflow" {
		if fl, ok := work["files"].([]any); ok {
			for _, f := range fl {
				if s, ok := f.(string); ok {
					files = append(files, s)
				}
			}
		}
		delete(work, "files")
		stages, _ := work["stages"].([]any)
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
		if len(uploaded) > 0 {
			work["inputs"] = uploaded
		}
	}
	body[key] = work
	out, _ := json.Marshal(body)
	resp, err := c.http.Post(c.base+"/schedules", "application/json", bytes.NewReader(out))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(answer)))
	}
	var sc scheduleView
	if err := json.Unmarshal(answer, &sc); err != nil {
		return err
	}
	next := "-"
	if sc.NextRun != nil {
		next = sc.NextRun.Local().Format("2006-01-02 15:04")
	}
	fmt.Printf("Schedule created: %s (%s %s, next run %s)\nSee it with: harnessctl schedule show %s\n", sc.ID, sc.Schedule, sc.Timezone, next, sc.ID)
	return nil
}

func (c *apiClient) cmdScheduleList() error {
	var list []scheduleView
	if err := c.get("/schedules", &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No schedules yet.")
		return nil
	}
	fmt.Printf("%-34s %-7s %-16s %-18s %-5s %-17s %s\n", "SCHEDULE ID", "STATE", "WHEN", "WHAT", "RUNS", "NEXT", "NAME")
	for _, sc := range list {
		next := "-"
		if sc.NextRun != nil {
			next = sc.NextRun.Local().Format("01-02 15:04")
		}
		fmt.Printf("%-34s %-7s %-16s %-18s %-5d %-17s %s\n", sc.ID, sc.State, truncate(sc.Schedule, 16), truncate(sc.Summary, 18), sc.Runs, next, sc.Name)
	}
	return nil
}

func (c *apiClient) cmdScheduleShow(id string) error {
	var sc scheduleView
	if err := c.get("/schedules/"+id, &sc); err != nil {
		return err
	}
	fmt.Printf("Schedule  %s %s\nState     %s\nWhen      %s (%s)\nRuns      %s %s\nPolicies  missed: %s, overlap: %s\n", sc.ID, sc.Name, sc.State, sc.Schedule, sc.Timezone, sc.Summary, "x"+fmt.Sprint(sc.Runs), sc.Missed, sc.Overlap)
	if sc.MaxRuns > 0 {
		fmt.Printf("Max runs  %d\n", sc.MaxRuns)
	}
	if sc.EndAt != nil {
		fmt.Printf("Ends      %s\n", sc.EndAt.Local().Format(time.RFC3339))
	}
	if sc.NextRun != nil {
		fmt.Printf("Next run  %s\n", sc.NextRun.Local().Format("2006-01-02 15:04"))
	}
	if len(sc.History) == 0 {
		fmt.Println("\nNo runs yet.")
		return nil
	}
	fmt.Printf("\n%-20s %-8s %-10s %s\n", "SLOT (UTC)", "OUTCOME", "STATE", "LINK / NOTE")
	for _, r := range sc.History {
		link := r.Reason
		switch {
		case r.Job != "":
			link = "job " + r.Job
		case r.Workflow != "":
			link = "workflow " + r.Workflow
		}
		if r.Manual {
			link = "(manual) " + link
		}
		fmt.Printf("%-20s %-8s %-10s %s\n", truncate(r.Slot, 20), r.Outcome, r.State, link)
	}
	return nil
}

func (c *apiClient) scheduleAction(id, action, done string) error {
	resp, err := c.http.Post(c.base+"/schedules/"+id+"/"+action, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Println(done)
	return nil
}

func (c *apiClient) cmdScheduleRunNow(id string) error {
	resp, err := c.http.Post(c.base+"/schedules/"+id+"/run", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var run scheduleRunView
	json.Unmarshal(b, &run)
	switch {
	case resp.StatusCode == http.StatusAccepted:
		fmt.Printf("Started: %s%s\n", run.Job, run.Workflow)
		return nil
	case run.Outcome == "failed":
		return fmt.Errorf("could not start: %s", run.Reason)
	}
	return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func (c *apiClient) cmdScheduleDelete(id string) error {
	req, _ := http.NewRequest(http.MethodDelete, c.base+"/schedules/"+id, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Println("Schedule deleted (runs already started carry on).")
	return nil
}
