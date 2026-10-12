package tasks

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ---- train.setup
//
// Makes a device able to run train.lora: a private Python environment with
// PyTorch (CPU) in <tools-dir>/train/venv, the base model in train/model,
// and the worker script. Everything is fixed here: the packages and their
// versions, the model, the script (built into the agent). The submitter
// chooses nothing, so this is no more than "install the training tools in
// their own folder", and it is off until the operator's policy turns it on
// (catalog OptIn) — the owner of a device that joined has agreed to run
// the harness's work, but a download of this size is a decision of its own.

//go:embed trainkit/lora_worker.py
var loraWorker []byte

const trainModelRepo = "HuggingFaceTB/SmolLM2-135M"

// trainModelScript downloads the base model into the current directory's model/.
const trainModelScript = `from huggingface_hub import snapshot_download
print("model", snapshot_download("` + trainModelRepo + `", local_dir="model", allow_patterns=["*.json", "*.safetensors", "*.txt"]))`

type trainSetup struct{ t *tools }

func pythonNames() []string {
	names := []string{"python3.12", "python3.11", "python3.10", "python3.9", "python3", "python"}
	if runtime.GOOS == "windows" {
		for i := range names {
			names[i] += ".exe"
		}
	}
	return names
}

// findPythons lists interpreters on PATH, best first, by looking at files
// (never exec.LookPath: HANDOVER §11).
func findPythons() []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range pythonNames() {
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if dir == "" {
				continue
			}
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// pythonVersion is "3.12" for an interpreter, or "" if it won't run.
func pythonVersion(ctx context.Context, py string) string {
	out, err := exec.CommandContext(ctx, py, "-c", `import sys; print("%d.%d" % sys.version_info[:2])`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// supportedPython: PyTorch has builds for these.
func supportedPython(v string) bool {
	switch v {
	case "3.9", "3.10", "3.11", "3.12":
		return true
	}
	return false
}

func (ts trainSetup) Available(context.Context) error {
	if runtime.GOOS == "android" {
		return errors.New("training tools can't be installed on Android")
	}
	if ts.t.dir == "" {
		return errors.New("no tools folder (-tools-dir)")
	}
	if len(findPythons()) == 0 {
		return errors.New("no Python on this device (install Python 3.9 to 3.12)")
	}
	return nil
}

// pipPackages are what the training environment holds: PyTorch (a CPU
// build) and the libraries tested with it. An Intel Mac can only have
// PyTorch 2.2.2, which needs the older libraries listed.
func pipPackages() (args []string, index string) {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return []string{"torch==2.2.2", "numpy<2", "transformers==4.44.2", "peft==0.12.0", "accelerate==0.34.2", "tokenizers<0.20", "huggingface_hub<0.26", "safetensors"}, ""
	case runtime.GOOS == "windows" || (runtime.GOOS == "linux" && runtime.GOARCH == "amd64"):
		return []string{"torch"}, "https://download.pytorch.org/whl/cpu"
	default:
		return []string{"torch"}, ""
	}
}

func (ts trainSetup) Run(ctx context.Context, env Env) error {
	dir := filepath.Join(ts.t.dir, "train")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(env.Stderr, format+"\n", a...) }

	py := ""
	for _, p := range findPythons() {
		if v := pythonVersion(ctx, p); supportedPython(v) {
			py = p
			logf("using %s (Python %s)", p, v)
			break
		}
	}
	if py == "" {
		return errors.New("no working Python 3.9 to 3.12 on this device")
	}
	if err := os.WriteFile(filepath.Join(dir, "lora_worker.py"), loraWorker, 0o644); err != nil {
		return err
	}
	venvPy := trainLoRA{ts.t}.python()
	if venvPy == "" {
		logf("creating the environment")
		if err := ts.run(ctx, env, dir, py, "-m", "venv", "venv"); err != nil {
			return fmt.Errorf("create the environment: %w", err)
		}
		if venvPy = (trainLoRA{ts.t}).python(); venvPy == "" {
			return errors.New("the environment has no Python")
		}
	}
	pkgs, index := pipPackages()
	pip := []string{"-m", "pip", "install", "--default-timeout", "180", "--retries", "10"}
	if index != "" {
		// PyTorch first, from its CPU index, then the rest from PyPI.
		logf("installing PyTorch (about 200 MB)")
		if err := ts.run(ctx, env, dir, venvPy, append(append(pip, pkgs...), "--index-url", index)...); err != nil {
			return fmt.Errorf("install PyTorch: %w", err)
		}
		pkgs = []string{"transformers", "peft", "safetensors", "numpy", "huggingface_hub"}
	}
	logf("installing the training libraries")
	if err := ts.run(ctx, env, dir, venvPy, append(pip, pkgs...)...); err != nil {
		return fmt.Errorf("install the libraries: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "model", "config.json")); err != nil {
		logf("downloading the base model %s (about 260 MB)", trainModelRepo)
		if err := ts.run(ctx, env, dir, venvPy, "-c", trainModelScript); err != nil {
			return fmt.Errorf("download the model: %w", err)
		}
	}
	var check bytes.Buffer
	cmd := exec.CommandContext(ctx, venvPy, "-c", "import torch, transformers, peft; print(torch.__version__, transformers.__version__, peft.__version__)")
	cmd.Stdout = &check
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the environment doesn't work after install: %w", err)
	}
	logf("ready: torch/transformers/peft %s", strings.TrimSpace(check.String()))
	return nil
}

// run is t.exec in the train folder (pip and the model download write there).
func (ts trainSetup) run(ctx context.Context, env Env, dir, prog string, args ...string) error {
	e := env
	e.Dir = dir
	return ts.t.exec(ctx, e, prog, args...)
}
