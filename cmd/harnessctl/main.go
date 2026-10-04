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
	"home-harness/internal/joinscript"
)

func main() {
	apiAddr := flag.String("api-addr", "http://127.0.0.1:7421", "manager API base URL")
	tokenFile := flag.String("token-file", "harness-operator-token", "the manager's operator token file (its -operator-token-file); the "+operatorTokenEnv+" environment variable overrides it")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	client := newAPIClient(strings.TrimRight(*apiAddr, "/"), loadOperatorToken(*tokenFile))
	var err error

	switch args[0] {
	case "nodes":
		err = client.cmdNodes()
	case "login-url":
		err = client.cmdLoginURL()
	case "rename":
		err = requireArgs(args, 3, "rename <id> <alias|->", func() error { return client.cmdRename(args[1], args[2]) })
	case "label":
		err = requireArgs(args, 3, "label <id> key=value|key= ...", func() error { return client.cmdLabel(args[1], args[2:]) })
	case "avail":
		err = requireArgs(args, 2, "avail <id> [auto|always|idle [minutes]|charging|paused] [hours HH:MM-HH:MM|hours -]", func() error { return client.cmdAvail(args[1], args[2:]) })
	case "audit":
		err = cmdAudit(client, args[1:])
	case "artifact":
		err = cmdArtifact(client, args[1:])
	case "artifacts":
		err = client.cmdArtifacts()
	case "outputs":
		err = cmdOutputs(client, args[1:])
	case "map":
		err = cmdMap(client, args[1:])
	case "tasks":
		err = client.cmdTasks(args[1:])
	case "models":
		err = client.cmdModels()
	case "ai":
		err = client.cmdAI()
	case "pull":
		err = requireArgs(args, 3, "pull <device-id> <model>", func() error {
			return client.cmdModelTask("llm.pull", args[1], args[2])
		})
	case "split":
		err = client.cmdSplit(args[1:])
	case "plan":
		err = cmdPlan(client, args[1:])
	case "plans":
		err = client.cmdPlans()
	case "rm-model":
		err = requireArgs(args, 3, "rm-model <device-id> <model>", func() error {
			return client.cmdModelTask("llm.remove", args[1], args[2])
		})
	case "ask":
		err = cmdAsk(client, args[1:])
	case "summarize", "summarise":
		err = cmdSummarize(client, args[1:])
	case "classify":
		err = cmdClassify(client, args[1:])
	case "embed":
		err = cmdEmbed(client, args[1:])
	case "do":
		err = cmdDo(client, args[1:])
	case "policy":
		err = cmdPolicy(client, args[1:])
	case "jobs":
		err = client.cmdJobs()
	case "job":
		err = requireArgs(args, 2, "job <job-id>", func() error { return client.cmdJob(args[1]) })
	case "job-outputs":
		err = cmdJobOutputs(client, args[1:])
	case "job-cancel":
		err = requireArgs(args, 2, "job-cancel <job-id>", func() error { return client.cmdJobCancel(args[1]) })
	case "workflow":
		err = cmdWorkflow(client, args[1:])
	case "workflows":
		err = client.cmdWorkflows()
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
	case "revoke":
		err = requireArgs(args, 2, "revoke <id>", func() error { return client.cmdRevokeNode(args[1]) })
	case "revocations":
		err = client.cmdRevocations()
	case "clear-suspect":
		err = requireArgs(args, 2, "clear-suspect <id>", func() error { return client.cmdClearSuspect(args[1]) })
	case "join-requests":
		err = client.cmdJoinRequests()
	case "join-window":
		err = client.cmdJoinWindow(args[1:])
	case "standby":
		err = client.cmdStandby(args[1:])
	case "approve":
		err = requireArgs(args, 2, "approve <id|code>", func() error { return client.cmdDecideJoin(strings.Join(args[1:], " "), true) })
	case "reject":
		err = requireArgs(args, 2, "reject <id|code>", func() error { return client.cmdDecideJoin(strings.Join(args[1:], " "), false) })
	case "unrevoke":
		err = requireArgs(args, 2, "unrevoke <id>", func() error { return client.cmdUnrevokeNode(args[1]) })
	case "join":
		err = requireArgs(args, 2, "join <manager-lan-addr|remote> [android]", func() error {
			platform := "windows"
			if len(args) >= 3 {
				platform = args[2]
			}
			mode, addr := joinscript.ModeLAN, args[1]
			if args[1] == "remote" {
				mode, addr = joinscript.ModeRemote, ""
			}
			return client.cmdJoin(mode, addr, platform)
		})
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
  login-url             print the dashboard sign-in link (it carries the
                        operator token: keep it private)
  rename <id> <alias|-> set the name the dashboard and "nodes" show for a
                        node ("-" clears it); the agent can't overwrite it
  label <id> key=value ...
                        set labels on a node; "key=" removes one
  avail <id> [auto|always|idle [minutes]|charging|paused] [hours 22:00-07:00|hours -]
                        when a device takes new work: auto (the default: not
                        while on battery), always, idle (after N minutes
                        without keyboard/mouse/screen use, default 5),
                        charging, or paused; "hours" limits any of them to a
                        daily window. Running work always finishes. With no
                        rule, shows the current one
  audit [-noise] [-n N] show the audit log, newest first: admissions (and
                        how), revocations, invitations, updates, sign-ins,
                        workloads; -noise shows rejections and reconnects
  run [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY]
      [-in [name=]FILE|name=sha256:HEX ...] [-out NAME ...] <id|-> <cmd> [args...]
                        submit a workload (id or "-" for auto-pick using v2's
                        resource-aware placement), print its ID. cmd/args are
                        passed directly to exec, not a shell — on Windows,
                        shell builtins like "echo" need "cmd /C echo ...".
                        "hostname" is a real .exe on both Windows and Unix
                        and proves the workload ran remotely. -min-mem takes
                        a size like "2GiB"; with an explicit id, the node is
                        still checked against any given requirements. This is
                        always the "system.execute" capability (v4).
                        -in uploads a local file (or names a stored one) into
                        the workload's working directory; -out names a file
                        it must leave there, fetched afterwards with
                        "outputs". Names are relative paths like in/a.csv.
                        The program runs with that directory as its cwd: run
                        scripts through their interpreter ("sh x.sh",
                        "powershell -File x.ps1", "python x.py").
  invoke [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY]
         <id|-> <capability> [key=value ...]
                        submit a workload invoking a named capability other
                        than plain command execution (v4), e.g.
                        "filesystem.read path=/some/file" — rejected with a
                        clear error if the target (or every node, for "-")
                        doesn't declare that capability. See "node <id>" for
                        what a node declares.
  outputs <workload-id> [dir]
                        download the files a workload delivered into dir
  artifact put <file>   store a file on the manager, print its sha256
  artifact get <sha256> [out-file]
                        download a stored file
  artifact rm <sha256>  delete a stored file (refused while a queued or
                        running workload needs it)
  artifacts             list stored files and how much space they use
  tasks [type]          list the typed task types (built into every agent, the
                        same on every OS, sandboxed to their files), whether
                        policy allows each and how many nodes offer it; with
                        a type, its parameters
  do [-target ID] [-in FILE ...] <type> [key=value ...]
                        run one typed task, e.g.
                          harnessctl do -in photo.jpg image.resize width=800
                          harnessctl do cpu.burn seconds=30
                        run, invoke, do and map take -priority high|normal|low
                        (default normal): when every device is busy, waiting
                        high-priority work starts first; waiting raises it
                        over time, and running work is never stopped
  models               list the local AI models (Ollama) the fleet's devices
                        have, and where
  ask [-model M] [-in FILE ...] [-target ID] "question"
                        ask a local model on whichever device has it; the
                        answer streams back as it is written (Ctrl-C cancels)
  summarize [-model M] [-prompt "..."] [-out summaries.md] FILE|GLOB ...
                        summarise every text file with a local model, one
                        task per file spread over the devices that have it,
                        into one report by file name, saved here (-dir)
  classify -labels a,b,c [-model M] [-out labels.csv] FILE|GLOB ...
                        sort every text file into one of your labels; the
                        answers land in one table (file, label)
  embed [-model M] [-out embeddings.json] FILE|GLOB ...
                        embeddings for every text file with an embedding
                        model (e.g. ollama pull embeddinggemma), joined into
                        one JSON file. Files over 32 KiB, or not UTF-8 text,
                        are skipped; Ctrl-C stops watching, not the job
  ai                    the base URL and API key for apps that speak OpenAI's
                        chat API (/v1/chat/completions), and the models now
  pull <device-id> <model>
                        download a model into that device's Ollama, e.g.
                          harnessctl pull node-1234 llama3.2:1b
  rm-model <device-id> <model>
                        remove a model from that device's Ollama
  split [start -main ID -model PATH.gguf -name NAME [-helpers ID,ID] | stop]
                        run one model too big for any single device across
                        several with llama.cpp (each needs llama.cpp
                        installed); chat with it through /v1 by NAME
  plan [-model M] [-in FILE ...] "what to do"
                        a local AI model plans a job from your words, using
                        only the built-in task types; the manager checks it
                        and nothing runs until you approve, e.g.
                          harnessctl plan -in a.jpg -in b.jpg "resize to 800 px and zip them"
  plan show|approve|reject <plan-id>
  plans                 list recent plans and the models that can plan
  policy                show what the fleet may run
  policy type <name> on|off | labels key=value,...|- | max-runtime 10m|0
                        enable/disable a type (raw system.execute and
                        filesystem.read are off until you turn them on),
                        limit it to nodes with these labels, or bound each
                        attempt's runtime
  policy type container.run images add|remove REF | images set REF,... | images -
                        | tags on|off | network on|off
                        which images containers may run (REF: a repository,
                        name@sha256:..., or registry/namespace/*), whether by
                        tag instead of digest, and whether with a network
  policy spot-check <percent>|off
                        re-run that share of each job's checkable tasks (at
                        least one) on a different device and compare the
                        results; a device whose result is the odd one out
                        is marked suspect (worth it once devices you don't
                        control join)
  clear-suspect <id>    clear a device's suspect mark once you looked into it
  map -type T [-each FILE|GLOB ... | -count N] [key=value ...]
      [-reduce-type T2 [-reduce-param key=value ...]]
                        a job of typed tasks, e.g.
                          harnessctl map -type image.resize -each "photos/*.jpg"
                            width=800 -reduce-type archive.zip
                            -reduce-param name=photos.zip
  map [-each FILE|GLOB ... | -count N] [-shared FILE ...] [-out NAME ...]
      [-attempts N] [-reduce "cmd args" [-reduce-out NAME ...]] <cmd> [args...]
                        run one task per file (or N copies) spread over the
                        fleet as a job: each task gets its file under its
                        base name ({in} in the args; {i} is its number),
                        failed tasks are retried elsewhere, and -reduce runs
                        once every task succeeded, with their outputs at
                        parts/<task>/<name>. Example:
                          harnessctl map -each "photos/*.jpg" -shared resize.py
                            -out small.jpg python resize.py {in} small.jpg
  jobs                  list jobs and their progress
  job <job-id>          show every task's state, attempts, node, and outputs
  job-outputs <job-id> [dir]
                        download the job's result (and each task's outputs
                        under parts/<task>/)
  job-cancel <job-id>   cancel a job and stop its running tasks
  workflow [-in FILE|GLOB ...] -step "TYPE [once|perFile|parts=N] [key=value ...]"
           [-combine "TYPE [key=value ...]"] -step ...
                        steps run in order, each on the results of the step
                        before (the first on the -in files): every step is
                        checked before anything runs, e.g.
                          harnessctl workflow -in "photos/*.jpg"
                            -step "image.resize perFile width=800"
                            -step "archive.zip name=photos.zip" -step file.hash
  workflow [-in FILE ...] FILE.json
                        the same from a file: {"name": "...", "files": [paths],
                        "stages": [{"type", "params", "mode", "parts",
                        "combine": {"type", "params"}}]}
  workflow show|cancel <workflow-id>
  workflow outputs <workflow-id> [dir]
                        download a finished workflow's results
  workflows             list workflows and how far each got
  workloads             list all known workloads
  workload <id>         show one workload's request, state, and captured output
  cancel <workload-id>  request cancellation of a running workload
  update <id>           push a self-update if the node isn't already running
                        the binary this manager currently serves (manager
                        started with -agent-binary); the node reconnects on
                        its own once done — no manual file transfer or
                        restart. "nodes" flags any node this would affect.
  revoke <id>           permanently refuse a node: it is disconnected,
                        forgotten, and rejected on every future REGISTER —
                        even with the shared pairing token its launcher
                        still holds. Its pinned workloads are canceled.
                        This denies one identity: a device that holds the
                        shared pairing token can mint a new one, so to cut
                        such a device off completely also rotate the
                        manager's -pairing-token.
  revocations           list revoked node identities
  join-requests         devices waiting to join by approval (they ran the
                        join page's installer, or an agent with -pair), with
                        the pairing code each one shows
  approve <id|code>     admit a waiting device: compare its pairing code
                        with the one the device shows first
  reject <id|code>      refuse a waiting device (for the next 10 minutes)
  join-window [open [minutes] | close]
                        whether new devices may join: open it (default 15
                        minutes) while adding a device, close it after.
                        Known devices always reconnect
  standby [add | status | promote [-force] | remove]
                        a standby manager: "add" (on the active manager)
                        prints the command that makes another machine its
                        standby — it copies this manager's whole state and
                        serves nothing until promoted; "promote" (on the
                        standby) takes over once the primary is gone
                        (-force: while it still runs, a planned switch-over)
  unrevoke <id>        lift a revocation; the node must then be admitted
                        afresh (its shared-token launcher does this on its
                        own; a one-time-invitation node needs a new one)
  join <manager-addr|remote> [windows|android|macos|linux]
                        print a ready-to-run onboarding block that
                        registers an agent — paste it into a terminal on
                        the new machine with no fingerprint lookup or
                        flag-typing. LAN mode also downloads the binary
                        directly from the manager. Default platform is
                        "windows" (a PowerShell block); "android" prints a
                        bash block for Termux instead (install Termux +
                        Termux:Boot from F-Droid first — printed with the
                        script). <manager-addr> is the manager's own -addr
                        value (e.g. 192.168.10.11:7420); requires the
                        manager to have been started with -agent-binary
                        pointed at the right build for the target
                        platform (the manager serves one binary at a
                        time — restart it with a different -agent-binary
                        to switch which platform "join" onboards).
                        Use "remote" instead of an address to generate a
                        relay-connected script from the manager's configured
                        -relay-addr/-relay-token. Because the relay does not
                        serve downloads, place agent.exe/agent on the remote
                        device before running that script.`)
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
	priority := priorityFlag(fs)
	var inputs, outputs fileList
	fs.Var(&inputs, "in", `input file, repeatable: "name=path", "path" (named after its base name), or "name=sha256:<hex>" for a stored file`)
	fs.Var(&outputs, "out", "output file the workload must produce in its working directory, repeatable (e.g. out/result.csv)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harnessctl run [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY] [-priority high|normal|low] [-in FILE ...] [-out NAME ...] <id|-> <command> [args...]")
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
	prio, err := checkPriority(*priority)
	if err != nil {
		return err
	}
	in, err := client.resolveInputs(inputs)
	if err != nil {
		return err
	}

	return client.cmdRunWorkload(target, rest[1], rest[2:], "", nil, req, *restart, in, outputs, prio)
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
	priority := priorityFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harnessctl invoke [-min-mem SIZE] [-min-cores N] [-max-cpu PCT] [-restart POLICY] [-priority high|normal|low] <id|-> <capability> [key=value ...]")
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
	prio, err := checkPriority(*priority)
	if err != nil {
		return err
	}

	return client.cmdRunWorkload(target, "", nil, capability, params, req, *restart, nil, nil, prio)
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
	base  string
	token string
	http  http.Client
}

// operatorTokenEnv overrides -token-file, e.g. on the phone:
// HARNESS_OPERATOR_TOKEN=$(cat ~/.home-harness/state/operator-token).
const operatorTokenEnv = "HARNESS_OPERATOR_TOKEN"

// loadOperatorToken returns the operator token from the environment or
// the manager's token file — "" if neither is available, which still
// works against a manager with authentication disabled and otherwise
// fails with bearerTransport's explanation.
func loadOperatorToken(path string) string {
	if token := strings.TrimSpace(os.Getenv(operatorTokenEnv)); token != "" {
		return token
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func newAPIClient(base, token string) *apiClient {
	c := &apiClient{base: base, token: token}
	c.http.Transport = &bearerTransport{token: token, base: http.DefaultTransport}
	return c
}

// bearerTransport sends the operator token on every request (including
// the long-lived /events stream) and turns a 401 into an actionable error
// instead of each command's generic "manager returned 401".
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if t.token == "" {
			return nil, fmt.Errorf("the manager requires its operator token: run harnessctl from the manager's directory, or pass -token-file or set %s", operatorTokenEnv)
		}
		return nil, fmt.Errorf("the manager rejected this operator token (was its token file replaced?); use the manager's current -operator-token-file")
	}
	return resp, err
}

// cmdLoginURL prints the dashboard sign-in link for this manager.
func (c *apiClient) cmdLoginURL() error {
	if c.token == "" {
		return fmt.Errorf("no operator token found: run from the manager's directory, or pass -token-file or set %s", operatorTokenEnv)
	}
	fmt.Println(c.base + "/#login=" + c.token)
	return nil
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
	// UpdateStatus is the manager's own platform-aware verdict
	// (internal/manager/selfupdate.go): current, available,
	// reinstall-required, or unknown.
	UpdateStatus string `json:"updateStatus"`

	Alias        string            `json:"alias"`
	Labels       map[string]string `json:"labels"`
	SameHostAs   []domain.NodeID   `json:"sameHostAs"`
	HostConflict bool              `json:"hostConflict"`
	// Slots is how many workloads the node runs at once; Running, how many
	// of them are in use.
	Slots   int `json:"slots"`
	Running int `json:"running"`
	// Availability: the operator's rule and whether the node takes new
	// work now (the manager's availability.go).
	Availability struct {
		Rule      domain.Availability `json:"rule"`
		Available bool                `json:"available"`
		Reason    string              `json:"reason"`
	} `json:"availability"`
	GPUs []domain.GPU `json:"gpus"`
	// Suspect: spot checks found its results differing (spotcheck.go).
	Suspect *domain.SuspectMark `json:"suspect"`
}

// displayName is the operator's alias when set, else the agent's name.
func (n nodeView) displayName() string {
	if n.Alias != "" {
		return n.Alias
	}
	return n.Name
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

	fmt.Printf("%-24s %-20s %-12s %-8s %-6s %-14s %s\n", "NODE ID", "NAME", "STATE", "CPU%", "BUSY", "LAST SEEN", "AGENT")
	for _, n := range nodes {
		lastSeen := "-"
		if !n.LastSeen.IsZero() {
			lastSeen = time.Since(n.LastSeen).Round(time.Second).String() + " ago"
		}
		agent := n.AgentVersion
		switch n.UpdateStatus {
		case "available":
			agent += " (outdated)"
		case "reinstall-required":
			agent += " (reinstall needed)"
		}
		// Same-machine hints are agent-asserted (see the manager's
		// fleet.go): shown for the operator to judge, never acted on.
		switch {
		case n.HostConflict:
			agent += " HOST-CONFLICT"
		case len(n.SameHostAs) > 0:
			agent += " SAME-HOST"
		}
		if n.State == domain.NodeReady && !n.Availability.Available {
			agent += "  [no new work: " + n.Availability.Reason + "]"
		}
		agent += suspectNote(n.Suspect)
		busy := fmt.Sprintf("%d/%d", n.Running, max(n.Slots, 1))
		fmt.Printf("%-24s %-20s %-12s %-8.1f %-6s %-14s %s\n",
			n.NodeID, truncate(n.displayName(), 20), n.State, n.Metrics.CPUPercent, busy, lastSeen, agent)
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
	fmt.Printf("Takes work     %s\n", describeAvailability(n))
	for _, g := range n.GPUs {
		mem := humanBytes(g.MemoryBytes)
		if g.Integrated {
			mem = "shares system memory"
		}
		fmt.Printf("GPU            %s (%s)\n", g.Name, mem)
	}
	if u := n.Metrics.Use; u != nil {
		fmt.Printf("Use            %s\n", describeUse(*u))
	}
	if m := n.Suspect; m != nil {
		fmt.Printf("Spot checks    %s (%d time(s), last %s); once looked into: harnessctl clear-suspect %s\n", m.Reason, m.Count, m.Last.Local().Format(time.RFC3339), n.NodeID)
	}

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
	Inputs        []domain.ArtifactRef        `json:"inputs,omitempty"`
	Outputs       []string                    `json:"outputs,omitempty"`
	OutputFiles   []domain.ArtifactRef        `json:"outputFiles,omitempty"`
	// Waiting: why a QUEUED workload hasn't started.
	Waiting  string          `json:"waiting,omitempty"`
	Priority domain.Priority `json:"priority,omitempty"`
}

func (c *apiClient) cmdRunWorkload(target, command string, args []string, capability string, params map[string]string, req domain.ResourceRequirements, restartPolicy string, inputs []map[string]string, outputs []string, priority string) error {
	reqBody, err := json.Marshal(map[string]any{
		"target": target, "command": command, "args": args,
		"capability": capability, "params": params,
		"requirements": req, "restartPolicy": restartPolicy,
		"inputs": inputs, "outputs": outputs, "priority": priority,
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

	var wl struct {
		domain.Workload
		State domain.WorkloadState `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wl); err != nil {
		return err
	}
	if wl.State == domain.WorkloadQueued {
		fmt.Printf("Workload queued: %s (every eligible node is busy; it starts as soon as one has a free slot)\n", wl.ID)
	} else {
		fmt.Printf("Workload submitted: %s (target: %s)\n", wl.ID, wl.Target)
	}
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
		command += priorityNote(w.Priority)
		if w.Waiting != "" {
			command += "  (waiting: " + w.Waiting + ")"
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
	if w.Priority != "" {
		fmt.Printf("Priority       %s\n", w.Priority)
	}
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
	for _, in := range w.Inputs {
		fmt.Printf("Input          %s (%s, %s)\n", in.Name, humanBytes(uint64(in.Size)), in.SHA256[:12])
	}
	delivered := map[string]domain.ArtifactRef{}
	for _, o := range w.OutputFiles {
		delivered[o.Name] = o
	}
	for _, name := range w.Outputs {
		if o, ok := delivered[name]; ok {
			fmt.Printf("Output         %s (%s, %s)\n", name, humanBytes(uint64(o.Size)), o.SHA256[:12])
		} else {
			fmt.Printf("Output         %s (not delivered)\n", name)
		}
	}
	if len(w.OutputFiles) > 0 {
		fmt.Printf("               fetch with: harnessctl outputs %s [dir]\n", w.ID)
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
	Fingerprint   string `json:"fingerprint"`
	PairingToken  string `json:"pairingToken"`
	Insecure      bool   `json:"insecure"`
	AgentBinaries []struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		SHA256       string `json:"sha256"`
		Path         string `json:"path"`
	} `json:"agentBinaries"`
	RelayAvailable bool   `json:"relayAvailable"`
	RelayAddr      string `json:"relayAddr"`
	RelayToken     string `json:"relayToken"`
}

// cmdJoin prints a ready-to-run onboarding block for the new machine (v5
// part 1: "windows", the default, a PowerShell block; "android": a bash
// block for Termux) — fetches GET /join-info (internal/manager/join.go)
// for the fingerprint/token (a trusted, loopback-scoped source — not the
// LAN discovery beacon, which is unauthenticated multicast and therefore
// spoofable) and hands off the actual script text to joinscript.Build
// (internal/joinscript), the single place that logic lives — the
// manager's own web dashboard (v5 part 2, GET /join-script) calls the
// same function in-process, so the CLI and the dashboard can never drift
// apart on script format.
func (c *apiClient) cmdJoin(mode joinscript.Mode, addr, platform string) error {
	var info joinInfoView
	if err := c.get("/join-info", &info); err != nil {
		return err
	}
	// Pick the catalog build for the target platform — never a single
	// "the" binary, which could belong to another platform.
	scriptInfo := joinscript.Info{
		Fingerprint:    info.Fingerprint,
		PairingToken:   info.PairingToken,
		Insecure:       info.Insecure,
		RelayAvailable: info.RelayAvailable,
		RelayAddr:      info.RelayAddr,
		RelayToken:     info.RelayToken,
	}
	if goos, arch, ok := joinscript.TargetPlatform(platform); ok {
		for _, b := range info.AgentBinaries {
			if b.OS == goos && b.Architecture == arch {
				scriptInfo.AgentBinaryAvailable = true
				scriptInfo.AgentBinarySHA256 = b.SHA256
				scriptInfo.AgentBinaryPath = b.Path
			}
		}
	} else if goos, unix := joinscript.UnixPlatformOS(platform); unix {
		// macOS/Linux scripts get every build for the OS and pick their
		// own CPU's at install time.
		scriptInfo.ArchBuilds = map[string]joinscript.ArchBuild{}
		for _, b := range info.AgentBinaries {
			if b.OS == goos {
				scriptInfo.ArchBuilds[b.Architecture] = joinscript.ArchBuild{SHA256: b.SHA256, Path: b.Path}
			}
		}
		scriptInfo.AgentBinaryAvailable = len(scriptInfo.ArchBuilds) > 0
	}
	script, err := joinscript.BuildMode(mode, addr, platform, scriptInfo)
	if err != nil {
		return err
	}
	fmt.Print(script)
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

func (c *apiClient) cmdRevokeNode(id string) error {
	resp, err := c.http.Post(c.base+"/nodes/"+id+"/revoke", "application/json", nil)
	if err != nil {
		return fmt.Errorf("revoke node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var revoked domain.RevokedNode
	if err := json.NewDecoder(resp.Body).Decode(&revoked); err != nil {
		return err
	}
	fmt.Printf("Revoked %s (%s). It has been disconnected and will be refused from now on.\n", revoked.NodeID, revoked.Name)
	fmt.Println("If that device holds the shared pairing token, also rotate the manager's -pairing-token to stop it enrolling a new identity.")
	return nil
}

func (c *apiClient) cmdRevocations() error {
	var revoked []domain.RevokedNode
	if err := c.get("/revocations", &revoked); err != nil {
		return err
	}
	if len(revoked) == 0 {
		fmt.Println("No revoked nodes.")
		return nil
	}
	fmt.Printf("%-24s %-20s %-16s %s\n", "NODE ID", "NAME", "PLATFORM", "REVOKED")
	for _, r := range revoked {
		platform := "-"
		if r.Platform.OS != "" {
			platform = r.Platform.OS + "/" + r.Platform.Architecture
		}
		fmt.Printf("%-24s %-20s %-16s %s\n", r.NodeID, truncate(r.Name, 20), platform, r.RevokedAt.Local().Format(time.RFC3339))
	}
	return nil
}

func (c *apiClient) cmdUnrevokeNode(id string) error {
	req, err := http.NewRequest(http.MethodDelete, c.base+"/revocations/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("unrevoke node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Printf("Revocation of %s lifted. The node must be admitted again: a shared-token launcher reconnects on its own, a device that joined by approval asks again while adding devices is open (harnessctl join-window open, then approve it: harnessctl join-requests), and a node enrolled by invitation needs a new one.\n", id)
	return nil
}

// putMeta replaces a node's operator metadata.
func (c *apiClient) putMeta(id string, meta map[string]any) error {
	body, _ := json.Marshal(meta)
	req, err := http.NewRequest(http.MethodPut, c.base+"/nodes/"+id+"/meta", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("set node metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// currentMeta fetches a node's alias and labels, since PUT replaces both.
func (c *apiClient) currentMeta(id string) (string, map[string]string, error) {
	var n nodeView
	if err := c.get("/nodes/"+id, &n); err != nil {
		return "", nil, err
	}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	return n.Alias, n.Labels, nil
}

func (c *apiClient) cmdRename(id, alias string) error {
	_, labels, err := c.currentMeta(id)
	if err != nil {
		return err
	}
	if alias == "-" {
		alias = ""
	}
	if err := c.putMeta(id, map[string]any{"alias": alias, "labels": labels}); err != nil {
		return err
	}
	fmt.Println("Renamed.")
	return nil
}

func (c *apiClient) cmdLabel(id string, pairs []string) error {
	alias, labels, err := c.currentMeta(id)
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return fmt.Errorf("invalid label %q (want key=value, or key= to remove)", pair)
		}
		if value == "" {
			delete(labels, key)
		} else {
			labels[key] = value
		}
	}
	if err := c.putMeta(id, map[string]any{"alias": alias, "labels": labels}); err != nil {
		return err
	}
	fmt.Println("Labels updated.")
	return nil
}

func cmdAudit(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	noise := fs.Bool("noise", false, "show rejected registrations and reconnects instead of operator actions and admissions")
	n := fs.Int("n", 30, "how many entries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fmt.Sprintf("/audit?limit=%d", *n) // operator actions + admissions
	if *noise {
		path += "&log=noise"
	}
	var entries []domain.AuditEntry
	if err := c.get(path, &entries); err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("No audit entries yet.")
		return nil
	}
	fmt.Printf("%-20s %-26s %-18s %-26s %s\n", "TIME", "KIND", "ACTOR", "NODE", "DETAIL")
	for _, e := range entries {
		detail, _ := json.Marshal(e.Detail)
		if e.Detail == nil {
			detail = []byte("-")
		}
		node := string(e.NodeID)
		if node == "" {
			node = "-"
		}
		fmt.Printf("%-20s %-26s %-18s %-26s %s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Kind, e.Actor, node, truncate(string(detail), 120))
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

// parseBytes parses a human-readable byte size like "2GiB" (see
// domain.ParseByteSize) — the inverse of humanBytes, for "-min-mem".
func parseBytes(s string) (uint64, error) { return domain.ParseByteSize(s) }

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
