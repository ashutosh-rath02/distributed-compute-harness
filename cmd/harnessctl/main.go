// Command harnessctl is the "simple CLI visibility" deliverable from
// v1.md §17: it talks to the manager's HTTP API to list nodes, inspect
// one node's resources/capabilities, dispatch basic commands, and tail
// the event stream. It is not the product — just a window into the
// harness.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"home-harness/internal/domain"
)

func main() {
	apiAddr := flag.String("api-addr", "http://127.0.0.1:7421", "manager API base URL")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	client := &apiClient{base: strings.TrimRight(*apiAddr, "/")}
	var err error

	switch args[0] {
	case "nodes":
		err = client.cmdNodes()
	case "node":
		err = requireArgs(args, 2, "node <id>", func() error { return client.cmdNode(args[1]) })
	case "resources":
		err = client.cmdResourceTotals()
	case "ping":
		err = requireArgs(args, 2, "ping <id>", func() error { return client.cmdCommand(args[1], domain.CommandPing, nil) })
	case "refresh":
		err = requireArgs(args, 2, "refresh <id>", func() error { return client.cmdCommand(args[1], domain.CommandRequestResourceRefresh, nil) })
	case "echo":
		err = requireArgs(args, 3, "echo <id> <message>", func() error {
			return client.cmdCommand(args[1], domain.CommandEcho, map[string]string{"message": args[2]})
		})
	case "status":
		err = requireArgs(args, 2, "status <id>", func() error { return client.cmdCommand(args[1], domain.CommandGetAgentStatus, nil) })
	case "info":
		err = requireArgs(args, 2, "info <id>", func() error { return client.cmdCommand(args[1], domain.CommandGetSystemInfo, nil) })
	case "events":
		err = client.cmdEvents()
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "harnessctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `harnessctl [-api-addr URL] <command> [args]

Commands:
  nodes                 list all known nodes
  node <id>             show one node's identity, state, resources, capabilities
  resources             show resource totals summed across all nodes
  ping <id>             send PING, print the result
  echo <id> <message>   send ECHO, print the echoed message
  status <id>           send GET_AGENT_STATUS, print the result
  info <id>             send GET_SYSTEM_INFO, print the result
  refresh <id>          send REQUEST_RESOURCE_REFRESH, print the result
  events                tail the harness event stream`)
}

func requireArgs(args []string, n int, usage string, fn func() error) error {
	if len(args) < n {
		return fmt.Errorf("usage: harnessctl %s", usage)
	}
	return fn()
}

type apiClient struct {
	base string
	http http.Client
}

type nodeView struct {
	NodeID       domain.NodeID       `json:"nodeId"`
	Name         string              `json:"name"`
	Hostname     string              `json:"hostname"`
	Platform     domain.Platform     `json:"platform"`
	AgentVersion string              `json:"agentVersion"`
	State        domain.NodeState    `json:"state"`
	LastSeen     time.Time           `json:"lastSeen"`
	Metrics      domain.RuntimeState `json:"metrics"`
}

func (c *apiClient) cmdNodes() error {
	var nodes []nodeView
	if err := c.get("/nodes", &nodes); err != nil {
		return err
	}
	if len(nodes) == 0 {
		fmt.Println("No nodes known to the harness yet.")
		return nil
	}
	fmt.Printf("%-24s %-20s %-12s %-8s %-14s %s\n", "NODE ID", "NAME", "STATE", "CPU%", "LAST SEEN", "AGENT")
	for _, n := range nodes {
		lastSeen := "-"
		if !n.LastSeen.IsZero() {
			lastSeen = time.Since(n.LastSeen).Round(time.Second).String() + " ago"
		}
		fmt.Printf("%-24s %-20s %-12s %-8.1f %-14s %s\n",
			n.NodeID, truncate(n.Name, 20), n.State, n.Metrics.CPUPercent, lastSeen, n.AgentVersion)
	}
	return nil
}

func (c *apiClient) cmdNode(id string) error {
	var n nodeView
	if err := c.get("/nodes/"+id, &n); err != nil {
		return err
	}
	fmt.Printf("Node ID        %s\n", n.NodeID)
	fmt.Printf("Name           %s\n", n.Name)
	fmt.Printf("Hostname       %s\n", n.Hostname)
	fmt.Printf("Platform       %s/%s\n", n.Platform.OS, n.Platform.Architecture)
	fmt.Printf("Agent version  %s\n", n.AgentVersion)
	fmt.Printf("State          %s\n", n.State)
	fmt.Printf("Last seen      %s\n", n.LastSeen.Format(time.RFC3339))
	fmt.Printf("CPU usage      %.1f%%\n", n.Metrics.CPUPercent)
	fmt.Printf("Mem available  %s\n", humanBytes(n.Metrics.MemoryAvailableBytes))

	var resources []domain.Resource
	if err := c.get("/nodes/"+id+"/resources", &resources); err == nil && len(resources) > 0 {
		fmt.Println("\nResources:")
		for _, r := range resources {
			fmt.Printf("  %-20s %s %s\n", r.Kind, strconv.FormatFloat(r.Capacity, 'f', -1, 64), r.Unit)
		}
	}

	var capabilities []domain.Capability
	if err := c.get("/nodes/"+id+"/capabilities", &capabilities); err == nil && len(capabilities) > 0 {
		fmt.Println("\nCapabilities:")
		for _, cap := range capabilities {
			fmt.Printf("  %-20s v%s\n", cap.Name, cap.Version)
		}
	}
	return nil
}

func (c *apiClient) cmdResourceTotals() error {
	var totals map[domain.ResourceKind]float64
	if err := c.get("/resources/total", &totals); err != nil {
		return err
	}
	fmt.Println("Total visible resources (informational only — not pooled):")
	for kind, total := range totals {
		fmt.Printf("  %-20s %s\n", kind, strconv.FormatFloat(total, 'f', -1, 64))
	}
	return nil
}

func (c *apiClient) cmdCommand(id string, name domain.CommandName, args map[string]string) error {
	reqBody, err := json.Marshal(map[string]any{"name": name, "args": args, "timeoutMs": 5000})
	if err != nil {
		return err
	}
	resp, err := c.http.Post(c.base+"/nodes/"+id+"/commands", "application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		return fmt.Errorf("send command: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var result domain.CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Success {
		fmt.Printf("FAILED: %s\n", result.Error)
		return nil
	}
	fmt.Println("OK")
	for k, v := range result.Output {
		fmt.Printf("  %-16s %s\n", k, v)
	}
	return nil
}

func (c *apiClient) cmdEvents() error {
	resp, err := c.http.Get(c.base + "/events")
	if err != nil {
		return fmt.Errorf("connect to event stream: %w", err)
	}
	defer resp.Body.Close()

	fmt.Println("Tailing harness events (Ctrl+C to stop)...")
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt domain.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
			continue
		}
		fmt.Printf("[%s] %-20s node=%s %v\n", evt.Timestamp.Format(time.RFC3339), evt.Type, evt.NodeID, evt.Data)
	}
	return scanner.Err()
}

func (c *apiClient) get(path string, v any) error {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
