package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"home-harness/internal/domain"
)

// statusJSON mirrors GET /status (internal/manager/status.go).
type statusJSON struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	Ready         bool   `json:"ready"`
	NotReady      string `json:"notReady"`
	Nodes         struct {
		Total      int            `json:"total"`
		TakingWork int            `json:"takingWork"`
		HeldBack   map[string]int `json:"heldBack"`
		List       []struct {
			ID                  string                   `json:"id"`
			Name                string                   `json:"name"`
			State               string                   `json:"state"`
			AgentVersion        string                   `json:"agentVersion"`
			Slots               int                      `json:"slots"`
			Running             int                      `json:"running"`
			HeartbeatAgeSeconds float64                  `json:"heartbeatAgeSeconds"`
			TakingWork          bool                     `json:"takingWork"`
			HeldBack            string                   `json:"heldBack"`
			Self                *domain.AgentSelfMetrics `json:"self"`
		} `json:"list"`
	} `json:"nodes"`
	Queue struct {
		Depth             int     `json:"depth"`
		OldestWaitSeconds float64 `json:"oldestWaitSeconds"`
	} `json:"queue"`
	Workloads      map[string]int `json:"workloads"`
	Jobs           map[string]int `json:"jobs"`
	Workflows      map[string]int `json:"workflows"`
	RecentFailures []struct {
		Workload   string    `json:"workload"`
		Capability string    `json:"capability"`
		Node       string    `json:"node"`
		Error      string    `json:"error"`
		At         time.Time `json:"at"`
	} `json:"recentFailures"`
	Standby struct {
		Configured  bool    `json:"configured"`
		Term        uint64  `json:"term"`
		SteppedDown bool    `json:"steppedDown"`
		LagSeconds  float64 `json:"lagSeconds"`
	} `json:"standby"`
	Disk struct {
		ArtifactUsedBytes  uint64 `json:"artifactUsedBytes"`
		ArtifactLimitBytes uint64 `json:"artifactLimitBytes"`
		StoreBytes         uint64 `json:"storeBytes"`
	} `json:"disk"`
}

// cmdStatus: one screen about the whole manager. (With a node ID,
// "status <id>" still asks that agent for its status.)
func (c *apiClient) cmdStatus() error {
	var s statusJSON
	if err := c.get("/status", &s); err != nil {
		return err
	}
	renderStatus(os.Stdout, s)
	return nil
}

// cmdTop is status again every few seconds, until Ctrl-C.
func (c *apiClient) cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	every := fs.Duration("n", 2*time.Second, "refresh interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for {
		var s statusJSON
		err := c.get("/status", &s)
		fmt.Print("\x1b[H\x1b[2J") // home, clear
		if err != nil {
			fmt.Println("harnessctl top:", err)
		} else {
			renderStatus(os.Stdout, s)
		}
		fmt.Printf("\nrefreshing every %s, Ctrl-C to quit\n", *every)
		time.Sleep(*every)
	}
}

func renderStatus(w io.Writer, s statusJSON) {
	ready := "ready"
	if !s.Ready {
		ready = "NOT READY: " + s.NotReady
	}
	fmt.Fprintf(w, "Manager %s (%s)  up %s  %s\n", s.Version, s.Commit, (time.Duration(s.UptimeSeconds) * time.Second).String(), ready)
	held := ""
	if len(s.Nodes.HeldBack) > 0 {
		var parts []string
		for k, n := range s.Nodes.HeldBack {
			if n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, strings.ReplaceAll(k, "_", " ")))
			}
		}
		sort.Strings(parts)
		if len(parts) > 0 {
			held = "  (held back: " + strings.Join(parts, ", ") + ")"
		}
	}
	fmt.Fprintf(w, "\nFleet: %d of %d devices taking work%s\n", s.Nodes.TakingWork, s.Nodes.Total, held)
	for _, n := range s.Nodes.List {
		note := ""
		switch {
		case n.State != "READY":
		case !n.TakingWork:
			note = "held: " + strings.ReplaceAll(n.HeldBack, "_", " ")
		}
		extra := ""
		if n.Self != nil {
			extra = fmt.Sprintf("  ran %d, failed %d", n.Self.WorkloadsRun, n.Self.WorkloadsFailed)
			if r := n.Self.CacheHitRate(); r >= 0 {
				extra += fmt.Sprintf(", cache %.0f%%", r*100)
			}
		}
		fmt.Fprintf(w, "  %-22s %-9s slots %d/%d  seen %3.0fs ago  v%-8s %s%s\n", truncate(n.Name, 22), n.State, n.Running, n.Slots, n.HeartbeatAgeSeconds, truncate(n.AgentVersion, 8), note, extra)
	}
	fmt.Fprintf(w, "\nQueue: %d waiting", s.Queue.Depth)
	if s.Queue.Depth > 0 {
		fmt.Fprintf(w, ", oldest %.0fs", s.Queue.OldestWaitSeconds)
	}
	fmt.Fprintf(w, "\nWork:  %s\n", counts(s.Workloads, "QUEUED", "PENDING", "RUNNING", "COMPLETED", "FAILED", "CANCELED"))
	fmt.Fprintf(w, "Jobs:  %s    Workflows: %s\n", counts(s.Jobs, "RUNNING", "COMPLETED", "FAILED"), counts(s.Workflows, "RUNNING", "COMPLETED", "FAILED"))
	if len(s.RecentFailures) > 0 {
		fmt.Fprintln(w, "\nRecent failures:")
		for _, f := range s.RecentFailures {
			fmt.Fprintf(w, "  %s  %-18s %-14s %s\n", f.At.Local().Format("15:04:05"), truncate(f.Capability, 18), truncate(f.Node, 14), truncate(f.Error, 60))
		}
	}
	fmt.Fprintln(w)
	switch {
	case s.Standby.SteppedDown:
		fmt.Fprintf(w, "Standby: this manager stepped down (term %d)\n", s.Standby.Term)
	case !s.Standby.Configured:
		fmt.Fprintf(w, "Standby: none (term %d)\n", s.Standby.Term)
	case s.Standby.LagSeconds < 0:
		fmt.Fprintf(w, "Standby: hasn't copied this manager yet (term %d)\n", s.Standby.Term)
	default:
		fmt.Fprintf(w, "Standby: copied %.0fs ago (term %d)\n", s.Standby.LagSeconds, s.Standby.Term)
	}
	fmt.Fprintf(w, "Disk:    files %s of %s, database %s\n", humanBytes(s.Disk.ArtifactUsedBytes), humanBytes(s.Disk.ArtifactLimitBytes), humanBytes(s.Disk.StoreBytes))
}

// counts renders "name n  name n" for the keys that are nonzero (or the
// first, so an idle line is not empty).
func counts(m map[string]int, keys ...string) string {
	var parts []string
	for _, k := range keys {
		if m[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", strings.ToLower(k), m[k]))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "  ")
}
