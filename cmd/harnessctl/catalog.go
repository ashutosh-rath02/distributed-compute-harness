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

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Typed tasks and policy (internal/catalog, internal/manager/policy.go).

type catalogEntry struct {
	catalog.Type
	Policy domain.TypePolicy `json:"policy"`
	Nodes  int               `json:"nodes"`
}

type catalogView struct {
	Types []catalogEntry                              `json:"types"`
	Raw   map[domain.CapabilityName]domain.TypePolicy `json:"raw"`
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (c *apiClient) cmdTasks(args []string) error {
	var cat catalogView
	if err := c.get("/catalog", &cat); err != nil {
		return err
	}
	if len(args) > 0 {
		for _, t := range cat.Types {
			if string(t.Name) == args[0] {
				printType(t)
				return nil
			}
		}
		return fmt.Errorf("no task type %q (see harnessctl tasks)", args[0])
	}
	fmt.Printf("%-16s %-26s %-6s %-6s %s\n", "TYPE", "TITLE", "POLICY", "NODES", "INPUT FILES")
	for _, t := range cat.Types {
		if t.Internal {
			continue // parts of split sessions (harnessctl split)
		}
		files := "-"
		if t.Inputs.Max > 0 {
			files = fmt.Sprintf("%d-%d", t.Inputs.Min, t.Inputs.Max)
		}
		fmt.Printf("%-16s %-26s %-6s %-6d %s\n", t.Name, truncate(t.Title, 26), onOff(t.Policy.Enabled), t.Nodes, files)
	}
	names := make([]string, 0, len(cat.Raw))
	for n := range cat.Raw {
		names = append(names, string(n))
	}
	sort.Strings(names)
	fmt.Println("\nRaw (advanced, unsandboxed):")
	for _, n := range names {
		fmt.Printf("  %-16s %s\n", n, onOff(cat.Raw[domain.CapabilityName(n)].Enabled))
	}
	fmt.Println("\nRun one with: harnessctl do <type> [key=value ...]   (details: harnessctl tasks <type>)")
	return nil
}

func printType(t catalogEntry) {
	fmt.Printf("%s v%s — %s\n%s\n\n", t.Name, t.Version, t.Title, t.Description)
	fmt.Printf("Policy   %s", onOff(t.Policy.Enabled))
	if len(t.Policy.NodeLabels) > 0 {
		fmt.Printf(", only on nodes labelled %v", t.Policy.NodeLabels)
	}
	if t.Policy.MaxRuntimeSeconds > 0 {
		fmt.Printf(", at most %ds per attempt", t.Policy.MaxRuntimeSeconds)
	}
	if t.Name == catalog.ContainerRun {
		fmt.Printf("\n         %s", containerPolicyLine(t.Policy))
	}
	fmt.Printf("\nNodes    %d ready node(s) offer it\n", t.Nodes)
	if t.Inputs.Max > 0 {
		ext := "any"
		if len(t.Inputs.Extensions) > 0 {
			ext = "." + strings.Join(t.Inputs.Extensions, ", .")
		}
		fmt.Printf("Inputs   %d-%d file(s) (%s) via -in\n", t.Inputs.Min, t.Inputs.Max, ext)
	}
	if len(t.Outputs) > 0 {
		fmt.Printf("Outputs  %s\n", strings.Join(t.Outputs, ", "))
	}
	if len(t.Params) > 0 {
		fmt.Println("\nParameters (key=value):")
		for _, p := range t.Params {
			limits := string(p.Type)
			if p.Type == catalog.Enum {
				limits = strings.Join(p.Enum, "|")
			}
			if p.Min != nil || p.Max != nil {
				limits += fmt.Sprintf(" %v..%v", deref(p.Min), deref(p.Max))
			}
			def := ""
			if p.Default != "" {
				def = " (default " + p.Default + ")"
			}
			fmt.Printf("  %-10s %-24s %s%s\n", p.Name, limits, p.Title, def)
		}
	}
}

func deref(f *float64) any {
	if f == nil {
		return ""
	}
	return *f
}

// cmdDo submits one typed task: harnessctl do [-target id] [-in FILE ...] <type> [key=value ...]
func cmdDo(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("do", flag.ContinueOnError)
	target := fs.String("target", "", "run on this node (default: the best one)")
	restart := fs.String("restart", "never", `restart policy: never, on-failure, always`)
	priority := priorityFlag(fs)
	var inputs fileList
	fs.Var(&inputs, "in", `input file, repeatable: "path", "name=path" or "name=sha256:<hex>"`)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harnessctl do [-target ID] [-priority high|normal|low] [-in FILE ...] <type> [key=value ...]   (see harnessctl tasks)")
	}
	pos, err := parseTyped(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		fs.Usage()
		return errors.New("missing the task type")
	}
	params, err := parseParams(pos[1:])
	if err != nil {
		return err
	}
	prio, err := checkPriority(*priority)
	if err != nil {
		return err
	}
	in, err := c.resolveInputs(inputs)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"target": *target, "capability": pos[0], "params": params, "inputs": in, "restartPolicy": *restart, "priority": prio})
	resp, err := c.http.Post(c.base+"/workloads", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var wl struct {
		ID     string `json:"id"`
		Target string `json:"target"`
		State  string `json:"state"`
	}
	json.NewDecoder(resp.Body).Decode(&wl)
	fmt.Printf("Task %s: %s (%s)\n", wl.State, wl.ID, wl.Target)
	fmt.Printf("Check it with: harnessctl workload %s   (files: harnessctl outputs %s)\n", wl.ID, wl.ID)
	return nil
}

// cmdPolicy: policy | policy type <name> on|off|labels k=v,..|-|max-runtime DURATION
func cmdPolicy(c *apiClient, args []string) error {
	var p domain.Policy
	if err := c.get("/policy", &p); err != nil {
		return err
	}
	if len(args) == 0 {
		var cat catalogView
		if err := c.get("/catalog", &cat); err != nil {
			return err
		}
		fmt.Printf("%-16s %-4s %-12s %s\n", "CAPABILITY", "ON", "MAX RUNTIME", "ONLY ON NODES LABELLED")
		show := func(name domain.CapabilityName, tp domain.TypePolicy) {
			rt := "-"
			if tp.MaxRuntimeSeconds > 0 {
				rt = (time.Duration(tp.MaxRuntimeSeconds) * time.Second).String()
			}
			labels := "-"
			if len(tp.NodeLabels) > 0 {
				labels = fmt.Sprint(tp.NodeLabels)
			}
			fmt.Printf("%-16s %-4s %-12s %s\n", name, onOff(tp.Enabled), rt, labels)
		}
		for _, t := range cat.Types {
			show(t.Name, t.Policy)
		}
		for _, name := range []domain.CapabilityName{domain.CapabilitySystemExecute, domain.CapabilityFilesystemRead} {
			show(name, cat.Raw[name])
		}
		for _, t := range cat.Types {
			if t.Name == catalog.ContainerRun {
				fmt.Printf("\n%s: %s\n", t.Name, containerPolicyLine(t.Policy))
			}
		}
		return nil
	}
	if len(args) < 3 || args[0] != "type" {
		return errors.New("usage: harnessctl policy [type <name> on|off | labels key=value,...|- | max-runtime DURATION]\n       " + containerPolicyUsage)
	}
	name := domain.CapabilityName(args[1])
	if p.Types == nil {
		p.Types = map[domain.CapabilityName]domain.TypePolicy{}
	}
	tp, ok := p.Types[name]
	if !ok {
		// Start from what applies now (a catalog type is on by default).
		var cat catalogView
		if err := c.get("/catalog", &cat); err != nil {
			return err
		}
		for _, t := range cat.Types {
			if t.Name == name {
				tp = t.Policy
			}
		}
		if r, ok := cat.Raw[name]; ok {
			tp = r
		}
	}
	switch args[2] {
	case "on", "off":
		tp.Enabled = args[2] == "on"
	case "labels":
		if len(args) < 4 {
			return errors.New("usage: harnessctl policy type <name> labels key=value,... (or - to clear)")
		}
		tp.NodeLabels = nil
		if args[3] != "-" {
			tp.NodeLabels = map[string]string{}
			for _, kv := range strings.Split(args[3], ",") {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" || v == "" {
					return fmt.Errorf("label %q must be key=value", kv)
				}
				tp.NodeLabels[k] = v
			}
		}
	case "max-runtime":
		if len(args) < 4 {
			return errors.New("usage: harnessctl policy type <name> max-runtime 10m (0 = no limit)")
		}
		d, err := time.ParseDuration(args[3])
		if args[3] == "0" {
			d, err = 0, nil
		}
		if err != nil {
			return fmt.Errorf("max-runtime: %w", err)
		}
		tp.MaxRuntimeSeconds = int(d / time.Second)
	default:
		if err := setContainerPolicy(name, &tp, args[2], args[3:]); err != nil {
			return err
		}
	}
	p.Types[name] = tp
	body, _ := json.Marshal(p)
	req, _ := http.NewRequest(http.MethodPut, c.base+"/policy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Printf("Policy for %s updated.\n", name)
	if name == domain.CapabilitySystemExecute && tp.Enabled && len(tp.NodeLabels) == 0 {
		fmt.Println("Raw commands now run on every device that offers them. Limit them with: harnessctl policy type system.execute labels raw=ok")
	}
	if name == catalog.ContainerRun {
		fmt.Println(containerPolicyLine(tp))
	}
	return nil
}

// parseTyped parses fs with flags allowed anywhere among the positional
// words of a typed command (a task type, key=value parameters — none of
// which start with '-'), so "do image.resize width=800 -in a.jpg" works
// like "do -in a.jpg image.resize width=800".
func parseTyped(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		i := 0
		for i < len(rest) && !strings.HasPrefix(rest[i], "-") {
			i++
		}
		pos = append(pos, rest[:i]...)
		if i == len(rest) {
			return pos, nil
		}
		args = rest[i:]
	}
}
