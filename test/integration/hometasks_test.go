package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/transport/ws"
)

// The home media and document task types (roadmap-after-9 item 14) on
// real agents. ffmpeg, whisper.cpp, poppler and tesseract are played by
// stand-ins (internal/tasks/testdata/faketools) that record their argv
// and write plausible outputs.

var homeTools struct {
	once sync.Once
	bin  []byte
	err  error
}

// homeToolsDir is a fresh tools folder holding the named stand-ins, plus
// whisper.cpp models (ggml-<name>.bin) for the given names.
func homeToolsDir(t *testing.T, tools []string, models ...string) string {
	t.Helper()
	homeTools.once.Do(func() {
		dir, err := os.MkdirTemp("", "home-tools-")
		if err != nil {
			homeTools.err = err
			return
		}
		defer os.RemoveAll(dir)
		exe := filepath.Join(dir, "faketool")
		out, err := exec.Command("go", "build", "-o", exe, "../../internal/tasks/testdata/faketools").CombinedOutput()
		if err != nil {
			homeTools.err = fmt.Errorf("build the stand-in tools: %v\n%s", err, out)
			return
		}
		homeTools.bin, homeTools.err = os.ReadFile(exe)
	})
	if homeTools.err != nil {
		t.Fatal(homeTools.err)
	}
	dir := t.TempDir()
	for _, n := range tools {
		if runtime.GOOS == "windows" {
			n += ".exe"
		}
		if err := os.WriteFile(filepath.Join(dir, n), homeTools.bin, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if len(models) > 0 {
		os.MkdirAll(filepath.Join(dir, "models"), 0o755)
		for _, m := range models {
			os.WriteFile(filepath.Join(dir, "models", "ggml-"+m+".bin"), []byte("model "+m), 0o600)
		}
	}
	return dir
}

// toolCalls are the stand-ins' recorded argv in a tools folder, without
// ffmpeg's -version probes.
func toolCalls(t *testing.T, dir string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c []string
		json.Unmarshal(sc.Bytes(), &c)
		if !(len(c) == 2 && c[1] == "-version") {
			out = append(out, c)
		}
	}
	return out
}

// startToolAgent is a file-capable agent with a tools folder (and no
// search of this machine's own install locations or PATH).
func startToolAgent(t *testing.T, ctx context.Context, m artifactManager, addr, name, tools string) (*agent.Agent, string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), name+"-work")
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn, OllamaURL: "127.0.0.1:1", ToolsDir: tools,
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: work,
		Name: name, HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond, MaxReconnectBackoff: 200 * time.Millisecond,
		HostFingerprint: "-", Insecure: true, CapabilityProbeInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	waitFor(t, 10*time.Second, func() bool {
		rec, ok := m.srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady && !rec.LastMetrics.LastHeartbeat.IsZero()
	})
	return a, work
}

// runTyped submits one typed task with one input and waits for it.
func runTyped(t *testing.T, api, capability string, params map[string]string, name string, content []byte) map[string]any {
	t.Helper()
	sha := uploadArtifact(t, api, content)
	code, out := postWorkload(t, api, map[string]any{"capability": capability, "params": params,
		"inputs": []map[string]string{{"name": name, "sha256": sha}}})
	if code != http.StatusAccepted {
		t.Fatalf("submit %s: %d %v", capability, code, out)
	}
	return waitState(t, api, out["id"].(string), 30*time.Second)
}

func outputFile(t *testing.T, api string, v map[string]any, i int) (string, []byte) {
	t.Helper()
	files, _ := v["outputFiles"].([]any)
	if len(files) <= i {
		t.Fatalf("no output %d: %v", i, v)
	}
	o := files[i].(map[string]any)
	resp, err := http.Get(api + "/artifacts/" + o["sha256"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return o["name"].(string), b
}

func catalogEntry(t *testing.T, api, name string) (nodes int, choices map[string][]string) {
	t.Helper()
	var cat struct {
		Types []struct {
			Name    string              `json:"name"`
			Nodes   int                 `json:"nodes"`
			Choices map[string][]string `json:"choices"`
		} `json:"types"`
	}
	getJSON(t, api+"/catalog", &cat)
	for _, ty := range cat.Types {
		if ty.Name == name {
			return ty.Nodes, ty.Choices
		}
	}
	t.Fatalf("%s isn't in the catalog", name)
	return 0, nil
}

func TestImageUpscaleRunsOnAnAgent(t *testing.T) {
	const addr = "127.0.0.1:19640"
	p := domain.DefaultPolicy()
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	readyAgent(t, ctx, m, addr, "upscaler")
	v := runTyped(t, m.api, "image.upscale", map[string]string{"scale": "3"}, "photo.png", pngBytes(60, 40))
	if v["state"] != "COMPLETED" {
		t.Fatalf("upscale: %v (%v)", v["state"], v["error"])
	}
	name, b := outputFile(t, m.api, v, 0)
	cfg, format, err := image.DecodeConfig(strings.NewReader(string(b)))
	if name != "photo-x3.png" || err != nil || format != "png" || cfg.Width != 180 || cfg.Height != 120 {
		t.Fatalf("output %s: %v %s %dx%d", name, err, format, cfg.Width, cfg.Height)
	}
}

func TestMediaTranscodeRunsOnTheDeviceWithFFmpeg(t *testing.T) {
	const addr = "127.0.0.1:19641"
	p := domain.DefaultPolicy() // raw commands off: none needed
	m := startPolicyManager(t, addr, &p)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	startToolAgent(t, ctx, m, addr, "no-tools", "")
	tools := homeToolsDir(t, []string{"ffmpeg"})
	media, work := startToolAgent(t, ctx, m, addr, "has-ffmpeg", tools)
	if n, _ := catalogEntry(t, m.api, "media.transcode"); n != 1 {
		t.Fatalf("media.transcode offered by %d devices, want 1", n)
	}
	// Several, so placement has a choice to make each time.
	for i := 0; i < 3; i++ {
		// A name starting with '-' must never reach ffmpeg as an option.
		v := runTyped(t, m.api, "media.transcode", map[string]string{"preset": "720p.mp4", "start": "3", "duration": "7"}, "-clip.mov", []byte(fmt.Sprintf("movie %d", i)))
		if v["state"] != "COMPLETED" || v["target"] != string(media.NodeID()) {
			t.Fatalf("transcode on %v: %v (%v)", v["target"], v["state"], v["error"])
		}
		name, b := outputFile(t, m.api, v, 0)
		if name != "-clip-720p.mp4" || string(b) != "mp4 made from -clip.mov (7 bytes)" {
			t.Fatalf("output %s %q", name, b)
		}
		dir := filepath.Join(work, v["id"].(string))
		want := []string{"ffmpeg", "-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-y",
			"-protocol_whitelist", "file", "-format_whitelist", "mov,mp4,matroska,webm,avi,mpeg,mpegts,flv,asf,gif,mp3,aac,wav,flac,ogg",
			"-ss", "3", "-i", "file:" + filepath.Join(dir, "-clip.mov"), "-t", "7",
			"-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale=-2:'min(720,trunc(ih/2)*2)'",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-f", "mp4", "file:" + filepath.Join(dir, "-clip-720p.mp4")}
		calls := toolCalls(t, tools)
		if len(calls) != i+1 || !slices.Equal(calls[i], want) {
			t.Fatalf("ffmpeg argv:\n got %q\nwant %q", calls, want)
		}
	}
	// The user never supplies ffmpeg's arguments: a made-up preset is
	// refused before anything runs.
	sha := uploadArtifact(t, m.api, []byte("x"))
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "media.transcode", "params": map[string]string{"preset": "-vf"},
		"inputs": []map[string]string{{"name": "a.mp4", "sha256": sha}}}); code != http.StatusBadRequest {
		t.Fatalf("raw preset: %d %v", code, out)
	}
}

func TestAudioTranscribeGoesToTheDeviceWithTheModel(t *testing.T) {
	const addr = "127.0.0.1:19642"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	wavOnly := homeToolsDir(t, []string{"whisper-cli"}, "base.en")
	withFFmpeg := homeToolsDir(t, []string{"whisper-cli", "ffmpeg"}, "small")
	a, aWork := startToolAgent(t, ctx, m, addr, "wav-only", wavOnly)
	b, _ := startToolAgent(t, ctx, m, addr, "with-ffmpeg", withFFmpeg)
	if n, choices := catalogEntry(t, m.api, "audio.transcribe"); n != 2 || !slices.Equal(choices["model"], []string{"base.en", "small"}) {
		t.Fatalf("audio.transcribe: %d devices, choices %v", n, choices)
	}

	// "small" is only on the device with ffmpeg, which converts the m4a.
	v := runTyped(t, m.api, "audio.transcribe", map[string]string{"model": "small", "language": "de"}, "memo.m4a", []byte("m4a"))
	if v["state"] != "COMPLETED" || v["target"] != string(b.NodeID()) {
		t.Fatalf("transcribe on %v: %v (%v)", v["target"], v["state"], v["error"])
	}
	name, txt := outputFile(t, m.api, v, 0)
	srtName, srt := outputFile(t, m.api, v, 1)
	if name != "transcript.txt" || string(txt) != "Hello from audio-16k.wav (de, model ggml-small.bin)\n" || srtName != "transcript.srt" || !strings.Contains(string(srt), "-->") {
		t.Fatalf("outputs %s %q, %s %q", name, txt, srtName, srt)
	}
	if !strings.Contains(v["stdout"].(string), "Hello from audio-16k.wav") {
		t.Fatalf("stdout %q", v["stdout"])
	}
	if calls := toolCalls(t, withFFmpeg); len(calls) != 2 || calls[0][0] != "ffmpeg" || calls[1][0] != "whisper-cli" ||
		calls[1][2] != filepath.Join(withFFmpeg, "models", "ggml-small.bin") || calls[1][4] != strings.TrimPrefix(calls[0][len(calls[0])-1], "file:") {
		t.Fatalf("calls %q", calls)
	}

	// "base.en": the WAV goes straight to whisper-cli.
	v = runTyped(t, m.api, "audio.transcribe", map[string]string{"model": "base.en"}, "memo.wav", []byte("RIFF....WAVE"))
	if v["state"] != "COMPLETED" || v["target"] != string(a.NodeID()) {
		t.Fatalf("transcribe on %v: %v (%v)", v["target"], v["state"], v["error"])
	}
	want := []string{"whisper-cli", "-m", filepath.Join(wavOnly, "models", "ggml-base.en.bin"), "-f", filepath.Join(aWork, v["id"].(string), "memo.wav"), "-l", "auto"}
	if calls := toolCalls(t, wavOnly); len(calls) != 1 || !slices.Equal(calls[0][:len(want)], want) {
		t.Fatalf("whisper argv %q, want it to start %q", calls, want)
	}

	// No device has "medium": refused up front, naming it.
	sha := uploadArtifact(t, m.api, []byte("RIFF"))
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "audio.transcribe", "params": map[string]string{"model": "medium"},
		"inputs": []map[string]string{{"name": "a.wav", "sha256": sha}}}); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out), "medium") {
		t.Fatalf("missing model: %d %v", code, out)
	}
}

func TestDocTextOCRGoesWhereTesseractIs(t *testing.T) {
	const addr = "127.0.0.1:19643"
	m := startPolicyManager(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	popplerOnly := homeToolsDir(t, []string{"pdftotext", "pdftoppm"})
	startToolAgent(t, ctx, m, addr, "poppler-only", popplerOnly)
	picture := pngBytes(30, 20)

	// Only pdftotext in the fleet: recognition is refused up front.
	sha := uploadArtifact(t, m.api, picture)
	if code, out := postWorkload(t, m.api, map[string]any{"capability": "doc.text", "params": map[string]string{"method": "ocr"},
		"inputs": []map[string]string{{"name": "receipt.png", "sha256": sha}}}); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out), "ocr") {
		t.Fatalf("ocr with no tesseract anywhere: %d %v", code, out)
	}
	v := runTyped(t, m.api, "doc.text", map[string]string{"method": "text"}, "letter.pdf", []byte("%PDF TEXT:Dear neighbour"))
	if _, text := outputFile(t, m.api, v, 0); v["state"] != "COMPLETED" || string(text) != "Dear neighbour\f" {
		t.Fatalf("text: %v (%v) %q", v["state"], v["error"], text)
	}

	everything := homeToolsDir(t, []string{"pdftotext", "pdftoppm", "tesseract"})
	ocr, _ := startToolAgent(t, ctx, m, addr, "ocr", everything)
	if _, choices := catalogEntry(t, m.api, "doc.text"); !slices.Equal(choices["method"], []string{"auto", "ocr", "text"}) {
		t.Fatalf("methods %v", choices)
	}
	for i := 0; i < 3; i++ {
		v := runTyped(t, m.api, "doc.text", map[string]string{"method": "ocr", "language": "deu"}, "receipt.png", picture)
		if _, text := outputFile(t, m.api, v, 0); v["state"] != "COMPLETED" || v["target"] != string(ocr.NodeID()) || string(text) != "recognized receipt.png (deu)\n" {
			t.Fatalf("ocr on %v: %v (%v) %q", v["target"], v["state"], v["error"], text)
		}
	}
	// auto (the default) needs everything, so a scan goes to that device
	// too, and is recognized page by page.
	v = runTyped(t, m.api, "doc.text", nil, "scan.pdf", []byte("%PDF PAGE PAGE"))
	if _, text := outputFile(t, m.api, v, 0); v["state"] != "COMPLETED" || v["target"] != string(ocr.NodeID()) ||
		string(text) != "recognized page 1 (eng)\n\frecognized page 2 (eng)\n\f" {
		t.Fatalf("scan on %v: %v (%v) %q", v["target"], v["state"], v["error"], text)
	}
	if calls := toolCalls(t, everything); len(calls) != 3+4 || calls[3][0] != "pdftotext" || calls[4][0] != "pdftoppm" || calls[5][0] != "tesseract" {
		t.Fatalf("calls %q", calls)
	}
	if calls := toolCalls(t, popplerOnly); len(calls) != 1 {
		t.Fatalf("the poppler-only device ran %q", calls)
	}
}
