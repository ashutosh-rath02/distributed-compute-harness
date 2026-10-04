package tasks

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// run compiles params against the catalog exactly as the manager and
// agent do, then runs the handler in a fresh directory holding files.
func run(t *testing.T, name domain.CapabilityName, params map[string]string, files map[string][]byte, order ...string) (Env, string, error) {
	t.Helper()
	ty, ok := catalog.Lookup(name)
	if !ok {
		t.Fatalf("no type %s", name)
	}
	dir := t.TempDir()
	var refs []domain.ArtifactRef
	for _, n := range order {
		p := filepath.Join(dir, filepath.FromSlash(n))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, files[n], 0o600); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, domain.ArtifactRef{Name: n, SHA256: strings.Repeat("0", 64)})
	}
	canon, outputs, err := ty.Compile(params, refs)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var stdout, stderr bytes.Buffer
	env := Env{Dir: dir, Params: canon, Inputs: order, Outputs: outputs, Stdout: &stdout, Stderr: &stderr}
	h, _ := Lookup(name)
	err = h.Run(context.Background(), env)
	return env, stdout.String(), err
}

func TestEveryCatalogTypeHasAHandler(t *testing.T) {
	full := NewRegistry(Options{OllamaURL: "127.0.0.1:1"}) // nothing listens there
	for _, ty := range catalog.Types() {
		if ty.Internal || ty.Name == catalog.ContainerRun {
			continue // split-session parts and containers: the agent registers their handlers (it owns the tunnels, it labels the containers)
		}
		if _, ok := full.Lookup(ty.Name); !ok {
			t.Errorf("catalog type %s has no handler", ty.Name)
		}
	}
	// Without a reachable Ollama only the self-contained types are offered.
	if got, want := len(full.Capabilities(context.Background())), len(builtinHandlers()); got != want {
		t.Fatalf("advertised %d types, want the %d built-ins", got, want)
	}
}

func TestIdentity(t *testing.T) {
	_, out, err := run(t, "system.identity", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if json.Unmarshal([]byte(out), &v) != nil || v["os"] == "" || v["cpus"] == nil {
		t.Fatalf("identity output %q", out)
	}
}

func TestCPUBurnHonorsTimeAndCancel(t *testing.T) {
	start := time.Now()
	if _, out, err := run(t, "cpu.burn", map[string]string{"seconds": "1", "threads": "2"}, nil); err != nil || !strings.Contains(out, "2 thread(s)") {
		t.Fatalf("burn: %v %q", err, out)
	}
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Fatalf("a 1s burn took %v", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	h, _ := Lookup("cpu.burn")
	if err := h.Run(ctx, Env{Params: map[string]string{"seconds": "60", "threads": "1"}, Stdout: &bytes.Buffer{}}); err == nil {
		t.Fatal("a canceled burn must report its context error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cpu.burn ignored cancellation")
	}
}

func testPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestImageResizeScalesAndKeepsAspect(t *testing.T) {
	files := map[string][]byte{"in/photo.png": testPNG(200, 100)}
	env, _, err := run(t, "image.resize", map[string]string{"width": "50", "format": "png"}, files, "in/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if env.Outputs[0] != "photo-50.png" {
		t.Fatalf("output name %q", env.Outputs[0])
	}
	f, _ := os.Open(env.out(0))
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || format != "png" || cfg.Width != 50 || cfg.Height != 25 {
		t.Fatalf("resized: %v %s %dx%d", err, format, cfg.Width, cfg.Height)
	}
	env, _, err = run(t, "image.resize", map[string]string{"width": "40", "height": "40"}, files, "in/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	f, _ = os.Open(env.out(0))
	cfg, format, err = image.DecodeConfig(f)
	f.Close()
	if err != nil || format != "jpeg" || cfg.Width != 40 || cfg.Height != 40 {
		t.Fatalf("jpg resize: %v %s %dx%d", err, format, cfg.Width, cfg.Height)
	}
	var jb bytes.Buffer
	jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil)
	if _, _, err := run(t, "image.resize", map[string]string{"width": "32"}, map[string][]byte{"cam.jpg": jb.Bytes()}, "cam.jpg"); err != nil {
		t.Fatalf("jpeg input: %v", err)
	}
}

// A tiny PNG whose header claims 50000x50000 must be refused from its
// header alone, before ~10 GB of pixels would be allocated.
func TestImageResizeRefusesDecompressionBombs(t *testing.T) {
	data := testPNG(1, 1)
	// IHDR data starts at byte 16 (8-byte signature, 4 length, 4 type).
	binary.BigEndian.PutUint32(data[16:], 50000)
	binary.BigEndian.PutUint32(data[20:], 50000)
	binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
	start := time.Now()
	_, _, err := run(t, "image.resize", map[string]string{"width": "100"}, map[string][]byte{"bomb.png": data}, "bomb.png")
	if err == nil || !strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("bomb: got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the bomb was decoded before being refused")
	}
	// And the result is capped too.
	if _, _, err := run(t, "image.resize", map[string]string{"width": "10000", "height": "10000"}, map[string][]byte{"s.png": testPNG(4, 4)}, "s.png"); err == nil || !strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("oversized result: got %v", err)
	}
}

func TestArchiveZipPacksByNameAndKeepsCollidingPaths(t *testing.T) {
	files := map[string][]byte{"parts/0000/a.jpg": []byte("one"), "parts/0001/a.jpg": []byte("two"), "notes.txt": []byte("three")}
	env, _, err := run(t, "archive.zip", map[string]string{"name": "all.zip"}, files, "parts/0000/a.jpg", "parts/0001/a.jpg", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(env.out(0))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		got[f.Name] = b.String()
		if strings.HasSuffix(f.Name, ".jpg") && f.Method != zip.Store {
			t.Errorf("%s: an already-compressed file should be stored", f.Name)
		}
	}
	if got["parts/0000/a.jpg"] != "one" || got["parts/0001/a.jpg"] != "two" || got["notes.txt"] != "three" || len(got) != 3 {
		t.Fatalf("zip entries %v", got)
	}
}

func TestFileHashMatchesKnownDigests(t *testing.T) {
	for alg, want := range map[string]string{
		"sha256": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		"md5":    "900150983cd24fb0d6963f7d28e17f72",
		"sha1":   "a9993e364706816aba3e25717850c26c9cd0d89d",
	} {
		env, _, err := run(t, "file.hash", map[string]string{"algorithm": alg}, map[string][]byte{"abc.txt": []byte("abc")}, "abc.txt")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(env.out(0))
		if string(b) != want+"  abc.txt\n" {
			t.Errorf("%s: %q", alg, b)
		}
	}
}

func TestTextCount(t *testing.T) {
	env, _, err := run(t, "text.count", nil, map[string][]byte{"a.txt": []byte("hello world\nfoo\n"), "b.txt": []byte("x")}, "a.txt", "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Files []counts `json:"files"`
		Total counts   `json:"total"`
	}
	b, _ := os.ReadFile(env.out(0))
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Files[0] != (counts{Name: "a.txt", Lines: 2, Words: 3, Bytes: 16}) || v.Total != (counts{Lines: 2, Words: 4, Bytes: 17}) {
		t.Fatalf("counts %+v", v)
	}
}
