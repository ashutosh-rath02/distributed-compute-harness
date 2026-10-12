package tasks

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Training a model across devices (train.lora, adapter.average), the way
// federated learning does it: every device trains a small adapter on its
// own share of the text for a number of steps, and the adapters are
// averaged. Only the adapters (hundreds of KB) travel, never the base
// model, which each device keeps in <tools-dir>/train/model. The training
// itself is a Python script the device owner installed there
// (tools/train/lora_worker.py with its own venv): like the media tools,
// the agent never downloads or installs it, and the script is chosen by
// the agent, never by the submitter.

// ---- train.lora

type trainLoRA struct{ t *tools }

func (tr trainLoRA) dir() string {
	if tr.t.dir == "" {
		return ""
	}
	return filepath.Join(tr.t.dir, "train")
}

// python is the venv's interpreter, if the device has one.
func (tr trainLoRA) python() string {
	d := tr.dir()
	if d == "" {
		return ""
	}
	for _, p := range []string{
		filepath.Join(d, "venv", "Scripts", "python.exe"),
		filepath.Join(d, "venv", "bin", "python"),
	} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

func (tr trainLoRA) Available(context.Context) error {
	d := tr.dir()
	if tr.python() == "" {
		return errors.New("no training environment (a venv with PyTorch in the tools folder's train/venv)")
	}
	if _, err := os.Stat(filepath.Join(d, "lora_worker.py")); err != nil {
		return errors.New("train/lora_worker.py isn't in the tools folder")
	}
	if _, err := os.Stat(filepath.Join(d, "model", "config.json")); err != nil {
		return errors.New("no base model in the tools folder's train/model")
	}
	return nil
}

// trainThreads: the task's own number, else a quarter of the logical
// cores (at least 2). More threads than a laptop has real cores run
// slower, not faster, and the owner can keep using the device.
func trainThreads(asked int) int {
	if asked > 0 {
		return min(asked, runtime.NumCPU())
	}
	return min(runtime.NumCPU(), max(2, runtime.NumCPU()/4))
}

// trainArgs is the worker's argument list: only files in the workload's
// directory and numbers the catalog already checked.
func trainArgs(script, model string, env Env, dataIn, adapterIn string) []string {
	args := []string{script, "--model", model, "--data", dataIn,
		"--adapter-out", env.out(0), "--metrics", env.out(1),
		"--steps", env.param("steps"), "--lr", env.param("lr"),
		"--threads", strconv.Itoa(trainThreads(env.intParam("threads")))}
	if adapterIn != "" {
		args = append(args, "--adapter-in", adapterIn)
	}
	return args
}

func (tr trainLoRA) Run(ctx context.Context, env Env) error {
	py := tr.python()
	if py == "" {
		return errors.New("this device's training environment is gone")
	}
	adapter := ""
	if len(env.Inputs) > 1 {
		adapter = env.in(1)
	}
	args := trainArgs(filepath.Join(tr.dir(), "lora_worker.py"), filepath.Join(tr.dir(), "model"), env, env.in(0), adapter)
	return tr.t.exec(ctx, env, py, args...)
}

// ---- adapter.average

type adapterAverage struct{}

func (adapterAverage) Available(ctx context.Context) error { return always(ctx) }

const (
	safetensorsMaxHeader = 16 << 20
	adapterMaxBytes      = 256 << 20
)

// safetensors is one parsed file: its tensors by name, as float32.
type safetensors struct {
	header map[string]stEntry
	data   []byte
}

type stEntry struct {
	DType   string   `json:"dtype"`
	Shape   []int64  `json:"shape"`
	Offsets [2]int64 `json:"data_offsets"`
}

func readSafetensors(path string) (*safetensors, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > adapterMaxBytes {
		return nil, fmt.Errorf("%s is larger than %d MB", filepath.Base(path), adapterMaxBytes>>20)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("%s is not a safetensors file", filepath.Base(path))
	}
	n := binary.LittleEndian.Uint64(raw[:8])
	if n > safetensorsMaxHeader || uint64(len(raw)) < 8+n {
		return nil, fmt.Errorf("%s has a damaged header", filepath.Base(path))
	}
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+n], &hdr); err != nil {
		return nil, fmt.Errorf("%s has a damaged header: %w", filepath.Base(path), err)
	}
	st := &safetensors{header: map[string]stEntry{}, data: raw[8+n:]}
	for k, v := range hdr {
		if k == "__metadata__" {
			continue
		}
		var e stEntry
		if err := json.Unmarshal(v, &e); err != nil {
			return nil, fmt.Errorf("tensor %s: %w", k, err)
		}
		if e.DType != "F32" {
			return nil, fmt.Errorf("tensor %s is %s; only float32 adapters can be averaged", k, e.DType)
		}
		count := int64(1)
		for _, d := range e.Shape {
			if d < 0 || (d > 0 && count > math.MaxInt32/d) {
				return nil, fmt.Errorf("tensor %s has a bad shape", k)
			}
			count *= d
		}
		if e.Offsets[0] < 0 || e.Offsets[1] != e.Offsets[0]+4*count || e.Offsets[1] > int64(len(st.data)) {
			return nil, fmt.Errorf("tensor %s has bad offsets", k)
		}
		st.header[k] = e
	}
	return st, nil
}

func (s *safetensors) floats(name string) []float32 {
	e := s.header[name]
	b := s.data[e.Offsets[0]:e.Offsets[1]]
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// averageAdapters returns the weighted mean of float32 safetensors files,
// all with the same tensor names and shapes, as one safetensors file.
func averageAdapters(files []*safetensors, weights []float64) ([]byte, error) {
	var names []string
	for k := range files[0].header {
		names = append(names, k)
	}
	sort.Strings(names)
	var sum float64
	for _, w := range weights {
		sum += w
	}
	if sum <= 0 {
		return nil, errors.New("the weights add up to nothing")
	}
	for i, f := range files[1:] {
		if len(f.header) != len(names) {
			return nil, fmt.Errorf("adapter %d has %d tensors, adapter 1 has %d", i+2, len(f.header), len(names))
		}
		for _, k := range names {
			b, ok := f.header[k]
			if !ok || fmt.Sprint(files[0].header[k].Shape) != fmt.Sprint(b.Shape) {
				return nil, fmt.Errorf("adapter %d has no tensor %s of the same shape", i+2, k)
			}
		}
	}
	hdr := map[string]stEntry{}
	var body []byte
	for _, k := range names {
		acc := make([]float64, len(files[0].floats(k)))
		for i, f := range files {
			w := weights[i] / sum
			for j, v := range f.floats(k) {
				acc[j] += w * float64(v)
			}
		}
		start := int64(len(body))
		for _, v := range acc {
			body = binary.LittleEndian.AppendUint32(body, math.Float32bits(float32(v)))
		}
		hdr[k] = stEntry{DType: "F32", Shape: files[0].header[k].Shape, Offsets: [2]int64{start, int64(len(body))}}
	}
	hb, err := json.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	for len(hb)%8 != 0 { // safetensors pads its header with spaces to 8 bytes
		hb = append(hb, ' ')
	}
	out := binary.LittleEndian.AppendUint64(nil, uint64(len(hb)))
	out = append(out, hb...)
	return append(out, body...), nil
}

func (adapterAverage) Run(ctx context.Context, env Env) error {
	// Only the adapters: a training task's metrics.json comes along in a
	// reduce's inputs and is not averaged.
	var order []int
	for i, name := range env.Inputs {
		if hasExt(name, "safetensors") {
			order = append(order, i)
		}
	}
	if len(order) == 0 {
		return errors.New("no .safetensors adapters among the inputs")
	}
	// Name order, like json.merge, so the weights line up with the parts
	// however the manager happened to list them.
	sort.SliceStable(order, func(a, b int) bool { return env.Inputs[order[a]] < env.Inputs[order[b]] })
	weights := make([]float64, len(order))
	for i := range weights {
		weights[i] = 1
	}
	if w := env.param("weights"); w != "" {
		parts := strings.Split(w, ",")
		if len(parts) != len(order) {
			return fmt.Errorf("%d weights for %d adapters", len(parts), len(order))
		}
		for i, p := range parts {
			v, err := strconv.ParseFloat(p, 64)
			if err != nil || v < 0 {
				return fmt.Errorf("weight %q is not a number", p)
			}
			weights[i] = v
		}
	}
	files := make([]*safetensors, 0, len(order))
	for _, i := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, err := readSafetensors(env.in(i))
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	out, err := averageAdapters(files, weights)
	if err != nil {
		return err
	}
	if err := os.WriteFile(env.out(0), out, 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stderr, "averaged %d adapters (%d tensors)\n", len(files), len(files[0].header))
	return err
}
