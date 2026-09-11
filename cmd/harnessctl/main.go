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
	"net"
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
	case "run":
		err = cmdRun(client, args[1:])
	case "invoke":
		err = cmdInvoke(client, args[1:])
	case "workloads":
		err = client.cmdWorkloads()
	case "workload":
		err = requireArgs(args, 2, "workload <id>", func() error { return client.cmdWorkload(args[1]) })
	case "cancel":
		err = requireArgs(args, 2, "cancel <workload-id>", func() error { return client.cmdCancelWorkload(args[1]) })
	case "update":
		err = requireArgs(args, 2, "update <id>", func() error { return client.cmdUpdateNode(args[1]) })
	case "join":
		err = requireArgs(args, 2, "join <manager-lan-addr>", func() error { return client.cmdJoin(args[1]) })
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
  events                tail the harness event stream
  run [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY] <id|-> <cmd> [args...]
                        submit a workload (id or "-" for auto-pick using v2's
                        resource-aware placement), print its ID. cmd/args are
                        passed directly to exec, not a shell — on Windows,
                        shell builtins like "echo" need "cmd /C echo ...".
                        "hostname" is a real .exe on both Windows and Unix
                        and proves the workload ran remotely. -min-mem takes
                        a size like "2GiB"; with an explicit id, the node is
                        still checked against any given requirements. This is
                        always the "system.execute" capability (v4).
  invoke [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY]
         <id|-> <capability> [key=value ...]
                        submit a workload invoking a named capability other
                        than plain command execution (v4), e.g.
                        "filesystem.read path=/some/file" — rejected with a
                        clear error if the target (or every node, for "-")
                        doesn't declare that capability. See "node <id>" for
                        what a node declares.
  workloads             list all known workloads
  workload <id>         show one workload's request, state, and captured output
  cancel <workload-id>  request cancellation of a running workload
  update <id>           push a self-update if the node isn't already running
                        the binary this manager currently serves (manager
                        started with -agent-binary); the node reconnects on
                        its own once done — no manual file transfer or
                        restart. "nodes" flags any node this would affect.
  join <manager-addr>   print a ready-to-run PowerShell block (v5) that
                        downloads agent.exe from this manager and registers
                        it — paste it into a terminal on any new LAN
                        machine to onboard it, with no manual file transfer,
                        fingerprint lookup, or flag-typing. <manager-addr>
                        is the manager's own -addr value (e.g.
                        192.168.10.11:7420); requires the manager to have
                        been started with -agent-binary.`)
}

// cmdRun parses "run"'s own flags separately from the top-level FlagSet,
// since flag.Parse stops at the first non-flag argument ("run" itself) and
// can't see subcommand-specific flags declared on the global FlagSet.
func cmdRun(client *apiClient, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	minMem := fs.String("min-mem", "", "minimum available memory required on the target node, e.g. 2GiB")
	minCores := fs.Float64("min-cores", 0, "minimum declared CPU cores required on the target node")
	maxCPU := fs.Float64("max-cpu", 0, "maximum acceptable live CPU load percent on the target node")
	restart := fs.String("restart", "never", `restart policy: "never" (default), "on-failure", or "always" — a non-"never" policy makes this a v3 "service" the manager keeps restarting after it stops`)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harnessctl run [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY] <id|-> <command> [args...]")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 2 {
		fs.Usage()
		return fmt.Errorf("missing required arguments")
	}
	target := rest[0]
	if target == "-" {
		target = ""
	}

	var req domain.ResourceRequirements
	if *minMem != "" {
		bytes, err := parseBytes(*minMem)
		if err != nil {
			return fmt.Errorf("-min-mem: %w", err)
		}
		req.MinMemoryBytes = bytes
	}
	req.MinCPUCores = *minCores
	req.MaxCPUPercent = *maxCPU

	if _, err := domain.ParseRestartPolicy(*restart); err != nil {
		return err
	}

	return client.cmdRunWorkload(target, rest[1], rest[2:], "", nil, req, *restart)
}

// cmdInvoke mirrors cmdRun's flag-parsing structure exactly, for a workload
// whose capability isn't plain command execution — Command/Args stay empty
// and the invocation's input travels in Params instead (parsed from
// trailing key=value positional args).
func cmdInvoke(client *apiClient, args []string) error {
	fs := flag.NewFlagSet("invoke", flag.ContinueOnError)
	minMem := fs.String("min-mem", "", "minimum available memory required on the target node, e.g. 2GiB")
	minCores := fs.Float64("min-cores", 0, "minimum declared CPU cores required on the target node")
	maxCPU := fs.Float64("max-cpu", 0, "maximum acceptable live CPU load percent on the target node")
	restart := fs.String("restart", "never", `restart policy: "never" (default), "on-failure", or "always"`)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harnessctl invoke [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY] <id|-> <capability> [key=value ...]")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 2 {
		fs.Usage()
		return fmt.Errorf("missing required arguments")
	}
	target := rest[0]
	if target == "-" {
		target = ""
	}
	capability := rest[1]

	params, err := parseParams(rest[2:])
	if err != nil {
		return err
	}

	var req domain.ResourceRequirements
	if *minMem != "" {
		bytes, err := parseBytes(*minMem)
		if err != nil {
			return fmt.Errorf("-min-mem: %w", err)
		}
		req.MinMemoryBytes = bytes
	}
	req.MinCPUCores = *minCores
	req.MaxCPUPercent = *maxCPU

	if _, err := domain.ParseRestartPolicy(*restart); err != nil {
		return err
	}

	return client.cmdRunWorkload(target, "", nil, capability, params, req, *restart)
}

// parseParams turns a list of "key=value" positional args into a map, for
// invoke's capability-specific input — there's no existing key=value parser
// elsewhere in this CLI to reuse.
func parseParams(args []string) (map[string]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	params := make(map[string]string, len(args))
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			return nil, fmt.Errorf("invalid param %q: expected key=value", arg)
		}
		params[key] = value
	}
	return params, nil
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
	BinaryHash   string              `json:"binaryHash,omitempty"`
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

	// Self-update outdated marker: one extra request for the manager's
	// currently-served hash, compared against each listed node's own —
	// closes the "no fleet-wide version visibility" gap without a
	// per-node comparison endpoint. currentHash stays "" (marker never
	// shown) if self-update is disabled or the request fails.
	var hashResp struct {
		SHA256 string `json:"sha256"`
	}
	_ = c.get("/agent-binary/hash", &hashResp)

	fmt.Printf("%-24s %-20s %-12s %-8s %-14s %s\n", "NODE ID", "NAME", "STATE", "CPU%", "LAST SEEN", "AGENT")
	for _, n := range nodes {
		lastSeen := "-"
		if !n.LastSeen.IsZero() {
			lastSeen = time.Since(n.LastSeen).Round(time.Second).String() + " ago"
		}
		agent := n.AgentVersion
		if hashResp.SHA256 != "" && n.BinaryHash != "" && n.BinaryHash != hashResp.SHA256 {
			agent += " (outdated)"
		}
		fmt.Printf("%-24s %-20s %-12s %-8.1f %-14s %s\n",
			n.NodeID, truncate(n.Name, 20), n.State, n.Metrics.CPUPercent, lastSeen, agent)
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

type workloadView struct {
	ID            domain.WorkloadID           `json:"id"`
	Target        domain.NodeID               `json:"target"`
	Command       string                      `json:"command"`
	Args          []string                    `json:"args,omitempty"`
	State         domain.WorkloadState        `json:"state"`
	Requirements  domain.ResourceRequirements `json:"requirements,omitempty"`
	RestartPolicy domain.RestartPolicy        `json:"restartPolicy,omitempty"`
	RestartCount  int                         `json:"restartCount,omitempty"`
	NextRestartAt time.Time                   `json:"nextRestartAt,omitempty"`
	Capability    domain.CapabilityName       `json:"capability,omitempty"`
	Params        map[string]string           `json:"params,omitempty"`
	Stdout        string                      `json:"stdout,omitempty"`
	Stderr        string                      `json:"stderr,omitempty"`
	Truncated     bool                        `json:"truncated,omitempty"`
	ExitCode      *int                        `json:"exitCode,omitempty"`
	Error         string                      `json:"error,omitempty"`
	StartedAt     time.Time                   `json:"startedAt,omitempty"`
	FinishedAt    time.Time                   `json:"finishedAt,omitempty"`
}

func (c *apiClient) cmdRunWorkload(target, command string, args []string, capability string, params map[string]string, req domain.ResourceRequirements, restartPolicy string) error {
	reqBody, err := json.Marshal(map[string]any{
		"target": target, "command": command, "args": args,
		"capability": capability, "params": params,
		"requirements": req, "restartPolicy": restartPolicy,
	})
	if err != nil {
		return err
	}
	resp, err := c.http.Post(c.base+"/workloads", "application/json", strings.NewReader(string(reqBody)))
	if err != nil {
		return fmt.Errorf("submit workload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var wl domain.Workload
	if err := json.NewDecoder(resp.Body).Decode(&wl); err != nil {
		return err
	}
	fmt.Printf("Workload submitted: %s (target: %s)\n", wl.ID, wl.Target)
	fmt.Printf("Check status with: harnessctl workload %s\n", wl.ID)
	return nil
}

func (c *apiClient) cmdWorkloads() error {
	var workloads []workloadView
	if err := c.get("/workloads", &workloads); err != nil {
		return err
	}
	if len(workloads) == 0 {
		fmt.Println("No workloads submitted yet.")
		return nil
	}
	fmt.Printf("%-34s %-24s %-10s %s\n", "WORKLOAD ID", "TARGET", "STATE", "COMMAND")
	for _, w := range workloads {
		command := w.Command
		if w.Capability != "" {
			command = string(w.Capability)
		}
		fmt.Printf("%-34s %-24s %-10s %s\n", w.ID, w.Target, w.State, command)
	}
	return nil
}

func (c *apiClient) cmdWorkload(id string) error {
	var w workloadView
	if err := c.get("/workloads/"+id, &w); err != nil {
		return err
	}
	fmt.Printf("Workload ID    %s\n", w.ID)
	fmt.Printf("Target         %s\n", w.Target)
	if w.Capability != "" {
		fmt.Printf("Capability     %s\n", w.Capability)
		for k, v := range w.Params {
			fmt.Printf("  %-12s %s\n", k, v)
		}
	} else {
		fmt.Printf("Command        %s %s\n", w.Command, strings.Join(w.Args, " "))
	}
	fmt.Printf("State          %s\n", w.State)
	if !w.Requirements.IsEmpty() {
		var parts []string
		if w.Requirements.MinCPUCores > 0 {
			parts = append(parts, fmt.Sprintf("min %.2f CPU cores", w.Requirements.MinCPUCores))
		}
		if w.Requirements.MinMemoryBytes > 0 {
			parts = append(parts, fmt.Sprintf("min %s available memory", humanBytes(w.Requirements.MinMemoryBytes)))
		}
		if w.Requirements.MaxCPUPercent > 0 {
			parts = append(parts, fmt.Sprintf("max %.1f%% CPU load", w.Requirements.MaxCPUPercent))
		}
		fmt.Printf("Requirements   %s\n", strings.Join(parts, ", "))
	}
	if w.RestartPolicy != domain.RestartNever {
		fmt.Printf("Restart policy %s\n", w.RestartPolicy)
		fmt.Printf("Restart count  %d\n", w.RestartCount)
		if !w.NextRestartAt.IsZero() {
			fmt.Printf("Next restart   %s\n", w.NextRestartAt.Format(time.RFC3339))
		}
	}
	if !w.StartedAt.IsZero() {
		fmt.Printf("Started        %s\n", w.StartedAt.Format(time.RFC3339))
	}
	if !w.FinishedAt.IsZero() {
		fmt.Printf("Finished       %s\n", w.FinishedAt.Format(time.RFC3339))
	}
	if w.Error != "" {
		fmt.Printf("Error          %s\n", w.Error)
	} else if w.ExitCode != nil {
		fmt.Printf("Exit code      %d\n", *w.ExitCode)
	}
	if w.Stdout != "" {
		fmt.Printf("\nStdout:\n%s\n", w.Stdout)
	}
	if w.Stderr != "" {
		fmt.Printf("\nStderr:\n%s\n", w.Stderr)
	}
	if w.Truncated {
		fmt.Println("(output truncated)")
	}
	return nil
}

func (c *apiClient) cmdCancelWorkload(id string) error {
	resp, err := c.http.Post(c.base+"/workloads/"+id+"/cancel", "application/json", nil)
	if err != nil {
		return fmt.Errorf("cancel workload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Println("Cancellation requested.")
	return nil
}

// joinInfoView mirrors manager.JoinInfo's JSON shape (internal/manager/join.go) —
// harnessctl duplicates the manager's small API view structs rather than
// importing internal/manager, matching this file's existing nodeView.
type joinInfoView struct {
	Fingerprint          string `json:"fingerprint"`
	PairingToken         string `json:"pairingToken"`
	Insecure             bool   `json:"insecure"`
	AgentBinaryAvailable bool   `json:"agentBinaryAvailable"`
	AgentBinarySHA256    string `json:"agentBinarySha256"`
}

// cmdJoin prints a ready-to-run PowerShell block for onboarding a new LAN
// machine (v5 part 1) — see GET /join-info (internal/manager/join.go) for
// where the embedded fingerprint/token come from, and why they must come
// from this trusted, loopback-scoped source rather than the LAN discovery
// beacon (which is unauthenticated multicast and therefore spoofable).
//
// The generated script uses curl.exe -k (bundled on Windows 10 1803+)
// rather than Invoke-WebRequest for its download: the brand-new machine
// has no pinned fingerprint yet (that's the whole point of this bootstrap
// step), so this one download necessarily runs with certificate chain
// validation off — exactly the reasoning v4's /agent-binary endpoint
// already documents (integrity comes from an explicit hash check, not the
// transport). The script verifies AgentBinarySHA256 immediately after
// downloading and refuses to run a binary that doesn't match.
func (c *apiClient) cmdJoin(addr string) error {
	// net.SplitHostPort(":7420") returns host="", nil error — a legal
	// listen-address form, but useless here: it's also the manager's own
	// -addr default and startup log text, so an operator copying that
	// literally would otherwise sail past this check and generate a
	// download URL pointing at nothing ("https://:7420/agent-binary").
	if host, _, err := net.SplitHostPort(addr); err != nil || host == "" {
		return fmt.Errorf("invalid manager address %q (want host:port, e.g. 192.168.10.11:7420)", addr)
	}

	var info joinInfoView
	if err := c.get("/join-info", &info); err != nil {
		return err
	}
	if !info.AgentBinaryAvailable {
		return fmt.Errorf("manager has no agent binary configured — restart it with -agent-binary to enable joining")
	}

	scheme := "https"
	curlFlag := "-k "
	authFlag := fmt.Sprintf("-manager-fingerprint %s", info.Fingerprint)
	if info.Insecure {
		scheme = "http"
		curlFlag = ""
		authFlag = "-insecure"
	}

	// The hash check and launch are one statement (if/else), not two
	// sequential lines: pasted into an interactive PowerShell session, each
	// top-level line runs independently, so a separate "if (...) { throw }"
	// followed by ".\agent.exe" would still launch the unverified binary
	// after the throw merely printed an error and moved on.
	fmt.Printf(`Paste this into a PowerShell terminal on the new machine:

curl.exe %s"%s://%s/agent-binary" -o agent.exe
if ((Get-FileHash agent.exe -Algorithm SHA256).Hash -ne "%s") { throw "agent.exe hash mismatch — download corrupted or tampered with, aborting" } else { .\agent.exe -manager-addr %s -pairing-token %s %s }
`, curlFlag, scheme, addr, strings.ToUpper(info.AgentBinarySHA256), addr, info.PairingToken, authFlag)
	return nil
}

func (c *apiClient) cmdUpdateNode(id string) error {
	resp, err := c.http.Post(c.base+"/nodes/"+id+"/update", "application/json", nil)
	if err != nil {
		return fmt.Errorf("update node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	// Two distinct response shapes share this 200: the already-up-to-date
	// short-circuit (map[string]string{"status": "already-up-to-date"}) and
	// the dispatch path's raw domain.CommandResult (a bool Success field,
	// which map[string]string can't hold) — decode loosely and branch on
	// whichever fields are actually present.
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if status, _ := result["status"].(string); status == "already-up-to-date" {
		fmt.Println("Already up to date.")
		return nil
	}
	if success, ok := result["success"].(bool); ok && !success {
		errMsg, _ := result["error"].(string)
		fmt.Printf("Update rejected: %s\n", errMsg)
		return nil
	}
	fmt.Println("Update accepted — the node will reconnect on its own once it's applied.")
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

// parseBytes parses a human-readable byte size like "2GiB", "512MB", "1024",
// or "1.5G" (case-insensitive, decimal and binary suffixes treated the
// same way) — the inverse of humanBytes, for the "-min-mem" flag.
func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q: no numeric value", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}

	suffix := strings.ToUpper(strings.TrimSpace(s[i:]))
	var mult float64
	switch suffix {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("invalid size %q: unrecognized unit %q", s, suffix)
	}
	return uint64(n * mult), nil
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
