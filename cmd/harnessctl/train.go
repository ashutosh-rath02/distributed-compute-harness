package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"home-harness/internal/domain"
)

const trainUsage = `usage: harnessctl train -data TEXT.txt [-rounds N] [-steps N] [-nodes ID,ID,...] [-lr 2e-4] [-threads N] [-out DIR]

Fine-tune a small language model on the text across your devices, the way
federated learning does it: every device trains a small adapter (LoRA) on
its own share of the text, the adapters are averaged, and the next round
starts from the average. Only adapters (about 2 MB) travel.

  -rounds   how many rounds (default 3)
  -steps    training steps on the fastest device each round (default 20); slower
            devices get fewer from round 2 on, so all finish together
  -nodes    which devices (default: every ready one that offers train.lora and takes work now)
  -threads  CPU threads per device (default 0: a quarter of its cores)
  -out      where the rounds' adapters and metrics are saved (default train-out)`

type trainMetrics struct {
	Steps      int     `json:"steps"`
	SecPerStep float64 `json:"secPerStep"`
	Threads    int     `json:"threads"`
	LossStart  float64 `json:"lossStart"`
	LossEnd    float64 `json:"lossEnd"`
}

// splitShards cuts text into n shards of about equal size, only at line ends.
func splitShards(text []byte, n int) [][]byte {
	if n < 1 {
		n = 1
	}
	shards := make([][]byte, 0, n)
	rest := text
	for i := 0; i < n; i++ {
		if i == n-1 {
			shards = append(shards, rest)
			break
		}
		want := len(rest) / (n - i)
		cut := want
		if nl := bytes.IndexByte(rest[want:], '\n'); nl >= 0 {
			cut = want + nl + 1
		} else {
			cut = len(rest)
		}
		shards = append(shards, rest[:cut])
		rest = rest[cut:]
	}
	return shards
}

// allocateSteps gives each device steps so that all take about as long as
// the fastest takes for base steps: steps ~ base * fastest/own seconds per
// step. A device whose speed isn't known yet gets base.
func allocateSteps(base int, secPerStep []float64) []int {
	fastest := math.Inf(1)
	for _, s := range secPerStep {
		if s > 0 && s < fastest {
			fastest = s
		}
	}
	out := make([]int, len(secPerStep))
	for i, s := range secPerStep {
		out[i] = base
		if s > 0 && !math.IsInf(fastest, 1) {
			out[i] = max(1, int(math.Round(float64(base)*fastest/s)))
		}
	}
	return out
}

func weightList(steps []int) string {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = strconv.Itoa(s)
	}
	return strings.Join(parts, ",")
}

func cmdTrain(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("train", flag.ContinueOnError)
	data := fs.String("data", "", "the text to train on")
	rounds := fs.Int("rounds", 3, "rounds")
	steps := fs.Int("steps", 20, "steps on the fastest device per round")
	nodesFlag := fs.String("nodes", "", "comma-separated node ids (default: every device that can)")
	lr := fs.String("lr", "2e-4", "learning rate (5e-5, 1e-4, 2e-4, 5e-4 or 1e-3)")
	threads := fs.Int("threads", 0, "CPU threads per device (0 = a quarter of its cores)")
	out := fs.String("out", "train-out", "folder for the results")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, trainUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" || fs.NArg() != 0 || *rounds < 1 || *steps < 1 {
		fs.Usage()
		return errors.New("give -data, and positive -rounds and -steps")
	}
	text, err := os.ReadFile(*data)
	if err != nil {
		return err
	}

	ids, names, err := trainNodes(c, *nodesFlag)
	if err != nil {
		return err
	}
	fmt.Printf("Training on %d device(s): %s\n", len(ids), strings.Join(names, ", "))
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	// Each device keeps its own share across rounds, so it is sent once
	// and the device's input cache serves every later round.
	var shardRefs []map[string]string
	for i, sh := range splitShards(text, len(ids)) {
		p := filepath.Join(*out, fmt.Sprintf("shard-%d.txt", i))
		if err := os.WriteFile(p, sh, 0o644); err != nil {
			return err
		}
		info, err := c.uploadFile(p)
		if err != nil {
			return fmt.Errorf("upload %s: %w", p, err)
		}
		shardRefs = append(shardRefs, map[string]string{"name": fmt.Sprintf("shard-%d.txt", i), "sha256": info.SHA256})
		fmt.Printf("  %s gets %s\n", names[i], humanBytes(uint64(len(sh))))
	}

	secPerStep := make([]float64, len(ids)) // unknown until round 1 has run
	var adapter map[string]string          // the average so far
	for r := 1; r <= *rounds; r++ {
		plan := allocateSteps(*steps, secPerStep)
		tasks := make([]map[string]any, len(ids))
		for i := range ids {
			inputs := []map[string]string{shardRefs[i]}
			if adapter != nil {
				inputs = append(inputs, adapter)
			}
			tasks[i] = map[string]any{
				"name": names[i], "target": string(ids[i]), "capability": "train.lora",
				"params": map[string]string{"steps": strconv.Itoa(plan[i]), "lr": *lr, "threads": strconv.Itoa(*threads)},
				"inputs": inputs,
			}
		}
		body := map[string]any{
			"name":        fmt.Sprintf("train round %d/%d", r, *rounds),
			"tasks":       tasks,
			"maxAttempts": 2,
			"reduce":      map[string]any{"capability": "adapter.average", "params": map[string]string{"weights": weightList(plan)}},
		}
		raw, _ := json.Marshal(body)
		resp, err := c.http.Post(c.base+"/jobs", "application/json", bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("submit round %d: %w", r, err)
		}
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("round %d: manager returned %s: %s", r, resp.Status, strings.TrimSpace(string(answer)))
		}
		var job jobView
		if err := json.Unmarshal(answer, &job); err != nil {
			return err
		}
		started := time.Now()
		fmt.Printf("\nRound %d/%d  (job %s)\n", r, *rounds, job.ID)
		for {
			time.Sleep(3 * time.Second)
			if err := c.get("/jobs/"+job.ID, &job); err != nil {
				return err
			}
			if job.State == "COMPLETED" || job.State == "FAILED" || job.State == "CANCELED" {
				break
			}
		}
		if job.State != "COMPLETED" {
			return fmt.Errorf("round %d ended %s: %s (see: harnessctl job %s)", r, job.State, job.Error, job.ID)
		}
		dir := filepath.Join(*out, fmt.Sprintf("round-%d", r))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		var lossStart, lossEnd, weight float64
		for i, t := range job.Tasks {
			for _, o := range t.Outputs {
				if o.Name != "metrics.json" {
					continue
				}
				p := filepath.Join(dir, fmt.Sprintf("metrics-%d.json", i))
				if _, err := c.download(o.SHA256, o.Name, p); err != nil {
					return err
				}
				var m trainMetrics
				b, _ := os.ReadFile(p)
				if json.Unmarshal(b, &m) != nil {
					continue
				}
				secPerStep[i] = m.SecPerStep
				fmt.Printf("  %-22s %3d steps, %.1fs/step, %d threads, loss %.3f -> %.3f\n", names[i], m.Steps, m.SecPerStep, m.Threads, m.LossStart, m.LossEnd)
				w := float64(m.Steps)
				lossStart, lossEnd, weight = lossStart+w*m.LossStart, lossEnd+w*m.LossEnd, weight+w
			}
		}
		var avg *domain.ArtifactRef
		for i := range job.Outputs {
			if job.Outputs[i].Name == "adapter.safetensors" {
				avg = &job.Outputs[i]
			}
		}
		if avg == nil {
			return fmt.Errorf("round %d produced no averaged adapter", r)
		}
		if _, err := c.download(avg.SHA256, avg.Name, filepath.Join(dir, "adapter.safetensors")); err != nil {
			return err
		}
		adapter = map[string]string{"name": "start.safetensors", "sha256": avg.SHA256}
		if weight > 0 {
			fmt.Printf("  average loss %.3f -> %.3f   (round took %s)\n", lossStart/weight, lossEnd/weight, time.Since(started).Round(time.Second))
		}
	}
	fmt.Printf("\nDone. The final adapter is %s\n", filepath.Join(*out, fmt.Sprintf("round-%d", *rounds), "adapter.safetensors"))
	return nil
}

// trainNodes picks the devices: the ones named, or every ready one that
// offers train.lora and takes new work now.
func trainNodes(c *apiClient, list string) ([]domain.NodeID, []string, error) {
	var nodes []nodeView
	if err := c.get("/nodes", &nodes); err != nil {
		return nil, nil, err
	}
	want := map[string]bool{}
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	sort.Slice(nodes, func(a, b int) bool { return nodes[a].NodeID < nodes[b].NodeID })
	var ids []domain.NodeID
	var names []string
	for _, n := range nodes {
		if len(want) > 0 && !want[string(n.NodeID)] {
			continue
		}
		if n.State != domain.NodeReady {
			if len(want) > 0 {
				return nil, nil, fmt.Errorf("%s is %s", n.NodeID, n.State)
			}
			continue
		}
		var caps []domain.Capability
		if err := c.get("/nodes/"+string(n.NodeID)+"/capabilities", &caps); err != nil {
			return nil, nil, err
		}
		has := false
		for _, cp := range caps {
			has = has || cp.Name == "train.lora"
		}
		if !has || (len(want) == 0 && !n.Availability.Available) {
			if len(want) > 0 {
				return nil, nil, fmt.Errorf("%s doesn't offer train.lora (no training environment installed there?)", n.NodeID)
			}
			continue
		}
		ids = append(ids, n.NodeID)
		name := n.Name
		if n.Alias != "" {
			name = n.Alias
		}
		names = append(names, name)
	}
	if len(ids) == 0 {
		return nil, nil, errors.New("no device offers train.lora right now (is the training environment installed on a ready device?)")
	}
	return ids, names, nil
}
