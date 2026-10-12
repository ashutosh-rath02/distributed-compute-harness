package tasks

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stFile builds a float32 safetensors file with the given tensors.
func stFile(t *testing.T, tensors map[string][]float32) []byte {
	t.Helper()
	hdr := map[string]stEntry{}
	var body []byte
	for _, name := range []string{"a", "b"} {
		v, ok := tensors[name]
		if !ok {
			continue
		}
		start := int64(len(body))
		for _, f := range v {
			body = binary.LittleEndian.AppendUint32(body, math.Float32bits(f))
		}
		hdr[name] = stEntry{DType: "F32", Shape: []int64{int64(len(v))}, Offsets: [2]int64{start, int64(len(body))}}
	}
	hb, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	out := binary.LittleEndian.AppendUint64(nil, uint64(len(hb)))
	return append(append(out, hb...), body...)
}

func readBack(t *testing.T, env Env) map[string][]float32 {
	t.Helper()
	st, err := readSafetensors(env.out(0))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]float32{}
	for k := range st.header {
		out[k] = st.floats(k)
	}
	return out
}

func TestAdapterAverageIsTheMean(t *testing.T) {
	files := map[string][]byte{
		"parts/0000/adapter.safetensors": stFile(t, map[string][]float32{"a": {1, 2, 3}, "b": {10}}),
		"parts/0001/adapter.safetensors": stFile(t, map[string][]float32{"a": {3, 4, 5}, "b": {20}}),
	}
	// Submission order is the reverse of name order; the result must not care.
	env, _, err := run(t, "adapter.average", nil, files, "parts/0001/adapter.safetensors", "parts/0000/adapter.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	got := readBack(t, env)
	if g := got["a"]; len(g) != 3 || g[0] != 2 || g[1] != 3 || g[2] != 4 {
		t.Fatalf("a = %v", got["a"])
	}
	if got["b"][0] != 15 {
		t.Fatalf("b = %v", got["b"])
	}
}

func TestAdapterAverageWeightsFollowNameOrder(t *testing.T) {
	files := map[string][]byte{
		"parts/0000/adapter.safetensors": stFile(t, map[string][]float32{"a": {0}}),
		"parts/0001/adapter.safetensors": stFile(t, map[string][]float32{"a": {10}}),
	}
	// Weight 1 on the first (by name) and 3 on the second: 7.5, whichever was submitted first.
	env, _, err := run(t, "adapter.average", map[string]string{"weights": "1,3"}, files, "parts/0001/adapter.safetensors", "parts/0000/adapter.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	if v := readBack(t, env)["a"][0]; v != 7.5 {
		t.Fatalf("weighted mean = %v, want 7.5", v)
	}
	if _, _, err := run(t, "adapter.average", map[string]string{"weights": "1,2,3"}, files, "parts/0000/adapter.safetensors", "parts/0001/adapter.safetensors"); err == nil || !strings.Contains(err.Error(), "weights") {
		t.Fatalf("a wrong number of weights must be refused, got %v", err)
	}
}

func TestAdapterAverageRefusesMismatchesAndJunk(t *testing.T) {
	good := stFile(t, map[string][]float32{"a": {1, 2}})
	cases := map[string][]byte{
		"other shape":  stFile(t, map[string][]float32{"a": {1, 2, 3}}),
		"other tensor": stFile(t, map[string][]float32{"b": {1, 2}}),
		"not a file":   []byte("hello"),
		"huge header":  binary.LittleEndian.AppendUint64(nil, 1<<40),
	}
	for name, bad := range cases {
		files := map[string][]byte{"parts/0000/x.safetensors": good, "parts/0001/x.safetensors": bad}
		if _, _, err := run(t, "adapter.average", nil, files, "parts/0000/x.safetensors", "parts/0001/x.safetensors"); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestTrainLoRAArgsAreFixedByTheAgent(t *testing.T) {
	env := Env{Dir: "/w", Params: map[string]string{"steps": "20", "lr": "2e-4", "threads": "0"}, Inputs: []string{"data.txt", "in.safetensors"}, Outputs: []string{"adapter.safetensors", "metrics.json"}}
	args := strings_join(trainArgs("/t/lora_worker.py", "/t/model", env, env.in(0), env.in(1)))
	for _, want := range []string{"--steps 20", "--lr 2e-4", "--adapter-in", "--model /t/model"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if strings.Contains(strings_join(trainArgs("s", "m", env, "d", "")), "--adapter-in") {
		t.Error("no starting adapter: no --adapter-in")
	}
}

func strings_join(a []string) string { return strings.Join(a, " ") }

func TestTrainLoRANeedsItsEnvironment(t *testing.T) {
	dir := t.TempDir()
	tr := trainLoRA{&tools{dir: dir}}
	if tr.Available(nil) == nil { //nolint:staticcheck // the context is unused
		t.Fatal("an empty tools folder must not offer training")
	}
	if (trainLoRA{&tools{}}).Available(nil) == nil { //nolint:staticcheck
		t.Fatal("no tools folder must not offer training")
	}
	for _, p := range []string{"train/venv/bin/python", "train/lora_worker.py", "train/model/config.json"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("x"), 0o755)
	}
	if err := tr.Available(nil); err != nil { //nolint:staticcheck
		t.Fatalf("a full environment must be offered: %v", err)
	}
}
