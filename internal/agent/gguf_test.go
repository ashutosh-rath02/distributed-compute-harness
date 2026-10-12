package agent

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func ggufStr(b *bytes.Buffer, s string) {
	binary.Write(b, binary.LittleEndian, uint64(len(s)))
	b.WriteString(s)
}

// ggufFile builds a header with a string, an array of strings, a float and
// then the layer count, the way real files order their entries.
func ggufFile(t *testing.T, blockType uint32, blocks any) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("GGUF")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	binary.Write(&b, binary.LittleEndian, uint64(0))
	binary.Write(&b, binary.LittleEndian, uint64(4))
	ggufStr(&b, "general.name")
	binary.Write(&b, binary.LittleEndian, uint32(8))
	ggufStr(&b, "a model")
	ggufStr(&b, "tokenizer.ggml.tokens")
	binary.Write(&b, binary.LittleEndian, uint32(9))
	binary.Write(&b, binary.LittleEndian, uint32(8))
	binary.Write(&b, binary.LittleEndian, uint64(3))
	for _, s := range []string{"a", "bb", "ccc"} {
		ggufStr(&b, s)
	}
	ggufStr(&b, "x.scale")
	binary.Write(&b, binary.LittleEndian, uint32(6))
	binary.Write(&b, binary.LittleEndian, float32(1.5))
	ggufStr(&b, "qwen2.block_count")
	binary.Write(&b, binary.LittleEndian, blockType)
	binary.Write(&b, binary.LittleEndian, blocks)
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGGUFBlockCountSkipsOtherEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		typ uint32
		v   any
	}{"uint32": {4, uint32(48)}, "int32": {5, int32(48)}, "uint64": {10, uint64(48)}, "uint8": {0, uint8(48)}} {
		n, err := ggufBlockCount(ggufFile(t, tc.typ, tc.v))
		if err != nil || n != 48 {
			t.Errorf("%s: %d, %v", name, n, err)
		}
	}
}

func TestGGUFBlockCountRefusesJunk(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "x.gguf")
	os.WriteFile(junk, []byte("hello, this is not a model"), 0o600)
	if _, err := ggufBlockCount(junk); err == nil {
		t.Error("junk accepted")
	}
	if _, err := ggufBlockCount(filepath.Join(t.TempDir(), "missing.gguf")); err == nil {
		t.Error("missing file accepted")
	}
	if _, err := ggufBlockCount(ggufFile(t, 4, uint32(0))); err == nil {
		t.Error("zero layers accepted")
	}
	if _, err := ggufBlockCount(ggufFile(t, 6, float32(48))); err == nil {
		t.Error("a float layer count accepted")
	}
}

// The model files on a developer's machine, when there are any.
func TestGGUFBlockCountOnRealModels(t *testing.T) {
	home, _ := os.UserHomeDir()
	for file, want := range map[string]int{"SmolLM2-135M-Instruct-Q8_0.gguf": 30, "gemma3-1b.gguf": 26, "Qwen2.5-14B-Instruct-Q4_K_M.gguf": 48} {
		p := filepath.Join(home, ".home-harness", "models", file)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		n, err := ggufBlockCount(p)
		if err != nil || n != want {
			t.Errorf("%s: %d layers, %v; want %d", file, n, err, want)
		}
		t.Logf("%s: %d layers", file, n)
	}
}

func TestOffloadLayers(t *testing.T) {
	const gb = int64(1) << 30
	cases := []struct {
		model, helpers int64
		blocks, want   int
	}{
		{9 * gb, 0, 48, 999},       // unknown: as before
		{9 * gb, 12 * gb, 48, 999}, // the helpers can hold all of it
		{9 * gb, 3 * gb, 48, 16},   // a third of it
		{9 * gb, 1, 48, 1},         // never zero: it is still a split
		{9 * gb, 3 * gb, 0, 999},   // layer count unknown
	}
	for _, c := range cases {
		if got := offloadLayers(c.model, c.blocks, c.helpers); got != c.want {
			t.Errorf("model %d helpers %d blocks %d: %d, want %d", c.model, c.helpers, c.blocks, got, c.want)
		}
	}
}
