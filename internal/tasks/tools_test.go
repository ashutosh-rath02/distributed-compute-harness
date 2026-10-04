package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

var fakeTools struct {
	once sync.Once
	dir  string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeTools.dir != "" {
		os.RemoveAll(fakeTools.dir)
	}
	os.Exit(code)
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// toolsDir builds the stand-ins (testdata/faketools) once per test run and
// puts the named ones in a fresh tools folder, under sub ("" = the folder
// itself, "bin", "ffmpeg-7.1/bin", ...).
func toolsDir(t *testing.T, sub string, names ...string) string {
	t.Helper()
	fakeTools.once.Do(func() {
		dir, err := os.MkdirTemp("", "faketools-")
		if err != nil {
			fakeTools.err = err
			return
		}
		fakeTools.dir = dir
		out, err := exec.Command("go", "build", "-o", filepath.Join(dir, exeName("faketool")), "./testdata/faketools").CombinedOutput()
		if err != nil {
			fakeTools.err = fmt.Errorf("build the stand-in tools: %v\n%s", err, out)
		}
	})
	if fakeTools.err != nil {
		t.Fatal(fakeTools.err)
	}
	bin, err := os.ReadFile(filepath.Join(fakeTools.dir, exeName("faketool")))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	at := filepath.Join(dir, filepath.FromSlash(sub))
	os.MkdirAll(at, 0o755)
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(at, exeName(n)), bin, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// calls are the stand-ins' argv, in order, as they recorded them (the
// program's name first).
func calls(t *testing.T, dir string) [][]string {
	t.Helper()
	var out [][]string
	f, err := os.Open(filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c []string
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// runs drops -version probes from calls.
func runs(cs [][]string) [][]string {
	var out [][]string
	for _, c := range cs {
		if !(len(c) == 2 && c[1] == "-version") {
			out = append(out, c)
		}
	}
	return out
}

func toolRegistry(dir string) *Registry {
	return NewRegistry(Options{OllamaURL: "127.0.0.1:1", ToolsDir: dir})
}

// runTool runs a tool-backed type from r like run does a built-in.
func runTool(t *testing.T, r *Registry, name domain.CapabilityName, params map[string]string, files map[string][]byte, order ...string) (Env, string, string, error) {
	t.Helper()
	ty, _ := catalog.Lookup(name)
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
	h, ok := r.Lookup(name)
	if !ok {
		t.Fatalf("no handler for %s", name)
	}
	err = h.Run(context.Background(), env)
	return env, stdout.String(), stderr.String(), err
}

func toolsAdvertised(r *Registry) map[domain.CapabilityName]map[string]string {
	out := map[domain.CapabilityName]map[string]string{}
	for _, c := range r.Capabilities(context.Background()) {
		out[c.Name] = c.Attributes
	}
	return out
}

func TestToolTypesAreOfferedOnlyWhereTheirProgramsAre(t *testing.T) {
	tool := []domain.CapabilityName{"media.transcode", "audio.transcribe", "doc.text"}
	// No tools folder and no system search: nothing, whatever this
	// machine has installed.
	none := toolsAdvertised(NewRegistry(Options{OllamaURL: "127.0.0.1:1"}))
	for _, n := range tool {
		if _, ok := none[n]; ok {
			t.Fatalf("%s offered with no tools", n)
		}
	}
	if _, ok := none["image.upscale"]; !ok {
		t.Fatal("image.upscale (pure Go) not offered")
	}

	// ffmpeg in an unpacked release's bin, one folder down.
	dir := toolsDir(t, "ffmpeg-7.1-essentials_build/bin", "ffmpeg")
	got := toolsAdvertised(toolRegistry(dir))
	if got["media.transcode"][catalog.AttrFFmpegVersion] != "7.1-fake" {
		t.Fatalf("media.transcode: %v", got["media.transcode"])
	}
	if _, ok := got["audio.transcribe"]; ok {
		t.Fatal("audio.transcribe offered without whisper-cli")
	}
	// The probe runs once per version of the file, not on every look.
	toolRegistry(dir).Capabilities(context.Background())
	r := toolRegistry(dir)
	r.Capabilities(context.Background())
	r.Capabilities(context.Background())
	if n := len(calls(t, filepath.Join(dir, "ffmpeg-7.1-essentials_build", "bin"))); n != 3 {
		t.Fatalf("%d -version probes for 3 registries, want 3", n)
	}

	// An ffmpeg that doesn't answer -version isn't offered.
	dir = toolsDir(t, "", "ffmpeg")
	os.WriteFile(filepath.Join(dir, "ffmpeg.broken"), nil, 0o600)
	broken := toolRegistry(dir)
	if _, ok := toolsAdvertised(broken)["media.transcode"]; ok {
		t.Fatal("a broken ffmpeg was offered")
	}
	// Fixed (say, its DLLs copied in afterwards): seen at the next look,
	// not only after a restart.
	os.Remove(filepath.Join(dir, "ffmpeg.broken"))
	if _, ok := toolsAdvertised(broken)["media.transcode"]; !ok {
		t.Fatal("a fixed ffmpeg wasn't offered")
	}

	// whisper-cli needs a model; voice-detection models don't count.
	dir = toolsDir(t, "bin", "whisper-cli")
	models := filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	os.WriteFile(filepath.Join(models, "ggml-silero-v5.1.2.bin"), []byte("vad"), 0o600)
	os.WriteFile(filepath.Join(models, "notes.txt"), []byte("x"), 0o600)
	if _, ok := toolsAdvertised(toolRegistry(dir))["audio.transcribe"]; ok {
		t.Fatal("audio.transcribe offered without a model")
	}
	os.WriteFile(filepath.Join(models, "ggml-small.bin"), []byte("model"), 0o600)
	os.WriteFile(filepath.Join(models, "ggml-base.en.bin"), []byte("model"), 0o600)
	if got := toolsAdvertised(toolRegistry(dir))["audio.transcribe"][catalog.AttrWhisperModels]; got != "base.en,small" {
		t.Fatalf("whisper models %q", got)
	}

	// doc.text says what it can do, so OCR work goes where tesseract is.
	for _, c := range []struct {
		tools          []string
		methods, found string
	}{
		{[]string{"pdftotext", "pdftoppm"}, "text", "pdftoppm,pdftotext"},
		{[]string{"tesseract"}, "ocr", "tesseract"},
		{[]string{"pdftotext", "tesseract"}, "ocr,text", "pdftotext,tesseract"},
		{[]string{"pdftotext", "pdftoppm", "tesseract"}, "auto,ocr,text", "pdftoppm,pdftotext,tesseract"},
	} {
		attrs, ok := toolsAdvertised(toolRegistry(toolsDir(t, "", c.tools...)))["doc.text"]
		if !ok || attrs[catalog.AttrDocMethods] != c.methods || attrs[catalog.AttrDocTools] != c.found {
			t.Errorf("%v: offered %v %v, want methods %q", c.tools, ok, attrs, c.methods)
		}
	}
	if _, ok := toolsAdvertised(toolRegistry(toolsDir(t, "", "pdftoppm")))["doc.text"]; ok {
		t.Fatal("doc.text offered with only pdftoppm")
	}
}

// Programs are found by checking files: on PATH only by absolute entries
// (a relative one would depend on the agent's working directory).
func TestToolsOnPathAreFoundByAbsolutePathOnly(t *testing.T) {
	dir := toolsDir(t, "", "pdftotext")
	tl := &tools{system: true}
	t.Setenv("PATH", dir)
	if got, want := tl.find("pdftotext"), filepath.Join(dir, exeName("pdftotext")); got != want && !inStandardDir(got) {
		t.Fatalf("found %q, want %q", got, want)
	}
	t.Chdir(dir)
	t.Setenv("PATH", ".")
	if got := tl.find("pdftotext"); got != "" && !inStandardDir(got) {
		t.Fatalf("found %q through a relative PATH entry", got)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(dir, "pdftotext"), 0o644)
		if got := (&tools{dir: dir}).find("pdftotext"); got != "" {
			t.Fatalf("found a file that isn't executable: %q", got)
		}
	}
}

// inStandardDir: this machine really has the program installed.
func inStandardDir(p string) bool {
	return p != "" && slices.Contains(standardToolDirs(), filepath.Dir(p))
}

func TestTranscodeRunsFFmpegWithFixedArguments(t *testing.T) {
	dir := toolsDir(t, "", "ffmpeg")
	r := toolRegistry(dir)
	// An input name starting with '-' is a valid file name: it must never
	// reach ffmpeg as an option.
	files := map[string][]byte{"-i.mov": []byte("movie")}
	env, out, _, err := runTool(t, r, "media.transcode", map[string]string{"preset": "720p.mp4", "start": "5", "duration": "20"}, files, "-i.mov")
	if err != nil {
		t.Fatal(err)
	}
	if env.Outputs[0] != "-i-720p.mp4" {
		t.Fatalf("output name %q", env.Outputs[0])
	}
	in, outPath := filepath.Join(env.Dir, "-i.mov"), filepath.Join(env.Dir, "-i-720p.mp4")
	want := []string{"ffmpeg", "-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-y",
		"-protocol_whitelist", "file", "-format_whitelist", "mov,mp4,matroska,webm,avi,mpeg,mpegts,flv,asf,gif,mp3,aac,wav,flac,ogg",
		"-ss", "5", "-i", "file:" + in, "-t", "20",
		"-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale=-2:'min(720,trunc(ih/2)*2)'",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-f", "mp4", "file:" + outPath}
	got := runs(calls(t, dir))
	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("ffmpeg argv:\n got %q\nwant %q", got, want)
	}
	if b, _ := os.ReadFile(outPath); string(b) != "mp4 made from -i.mov (5 bytes)" || !strings.Contains(out, "-i-720p.mp4") {
		t.Fatalf("output %q, stdout %q", b, out)
	}

	// Audio: no start or length unless asked.
	env, _, _, err = runTool(t, r, "media.transcode", map[string]string{"preset": "audio.mp3"}, map[string][]byte{"song.flac": []byte("x")}, "song.flac")
	if err != nil {
		t.Fatal(err)
	}
	got = runs(calls(t, dir))
	last := got[len(got)-1]
	if i := slices.Index(last, "-i"); i < 0 || slices.Contains(last, "-ss") || slices.Contains(last, "-t") ||
		!slices.Equal(last[i+2:], []string{"-map", "0:a:0", "-vn", "-c:a", "libmp3lame", "-q:a", "2", "-f", "mp3", "file:" + filepath.Join(env.Dir, "song-audio.mp3")}) {
		t.Fatalf("mp3 argv %q", last)
	}

	// A GIF is a clip: 10 seconds unless asked, never more than 30.
	if _, _, _, err := runTool(t, r, "media.transcode", map[string]string{"preset": "clip.gif"}, map[string][]byte{"a.mp4": []byte("x")}, "a.mp4"); err != nil {
		t.Fatal(err)
	}
	got = runs(calls(t, dir))
	if last := got[len(got)-1]; !slices.Contains(last, "-t") || last[slices.Index(last, "-t")+1] != "10" || last[len(last)-2] != "gif" {
		t.Fatalf("gif argv %q", last)
	}
	before := len(calls(t, dir))
	if _, _, _, err := runTool(t, r, "media.transcode", map[string]string{"preset": "clip.gif", "duration": "31"}, map[string][]byte{"a.mp4": []byte("x")}, "a.mp4"); err == nil || !strings.Contains(err.Error(), "at most 30 seconds") {
		t.Fatalf("31 s GIF: %v", err)
	}
	if len(calls(t, dir)) != before {
		t.Fatal("ffmpeg ran for a refused GIF")
	}

	// A failing ffmpeg fails the task with what it said (the registry
	// still remembers it answering -version).
	os.WriteFile(filepath.Join(dir, "ffmpeg.broken"), nil, 0o600)
	if _, _, _, err := runTool(t, r, "media.transcode", nil, map[string][]byte{"a.mp4": []byte("x")}, "a.mp4"); err == nil || !strings.Contains(err.Error(), "ffmpeg failed") || !strings.Contains(err.Error(), "broken on purpose") {
		t.Fatalf("broken ffmpeg: %v", err)
	}
}

func TestTranscribeConvertsWithFFmpegThenRunsWhisper(t *testing.T) {
	dir := toolsDir(t, "", "whisper-cli", "ffmpeg")
	os.MkdirAll(filepath.Join(dir, "models"), 0o755)
	model := filepath.Join(dir, "models", "ggml-base.en.bin")
	os.WriteFile(model, []byte("model"), 0o600)
	r := toolRegistry(dir)
	env, out, _, err := runTool(t, r, "audio.transcribe", map[string]string{"model": "base.en", "language": "en"}, map[string][]byte{"-talk.mp3": []byte("mp3")}, "-talk.mp3")
	if err != nil {
		t.Fatal(err)
	}
	got := runs(calls(t, dir))
	if len(got) != 2 || got[0][0] != "ffmpeg" || got[1][0] != "whisper-cli" {
		t.Fatalf("calls %q", got)
	}
	conv := got[0]
	wav := strings.TrimPrefix(conv[len(conv)-1], "file:")
	if filepath.Dir(filepath.Dir(wav)) != env.Dir || filepath.Base(wav) != "audio-16k.wav" ||
		!slices.Equal(conv[slices.Index(conv, "-i"):], []string{"-i", "file:" + filepath.Join(env.Dir, "-talk.mp3"),
			"-map", "0:a:0", "-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-f", "wav", "file:" + wav}) {
		t.Fatalf("conversion argv %q", conv)
	}
	base := filepath.Join(filepath.Dir(wav), "transcript")
	want := []string{"whisper-cli", "-m", model, "-f", wav, "-l", "en", "-t", fmt.Sprint(whisperThreads()), "-otxt", "-osrt", "-of", base, "-np"}
	if !slices.Equal(got[1], want) {
		t.Fatalf("whisper argv:\n got %q\nwant %q", got[1], want)
	}
	txt, _ := os.ReadFile(env.out(0))
	srt, _ := os.ReadFile(env.out(1))
	if env.Outputs[0] != "transcript.txt" || env.Outputs[1] != "transcript.srt" ||
		string(txt) != "Hello from audio-16k.wav (en, model ggml-base.en.bin)\n" || !strings.HasPrefix(string(srt), "1\n00:00:00,000 --> ") {
		t.Fatalf("outputs %q %q", txt, srt)
	}
	if !strings.Contains(out, "[00:00:00.000 --> 00:00:02.000]   Hello from") {
		t.Fatalf("stdout %q", out)
	}
	if _, err := os.Stat(filepath.Dir(wav)); !os.IsNotExist(err) {
		t.Fatal("the converted recording was left behind")
	}

	// A model the device doesn't have is refused; the name never becomes
	// a path.
	if _, _, _, err := runTool(t, r, "audio.transcribe", map[string]string{"model": "small"}, map[string][]byte{"a.wav": []byte("RIFF")}, "a.wav"); err == nil || !strings.Contains(err.Error(), `no whisper model "small"`) {
		t.Fatalf("missing model: %v", err)
	}

	// Without ffmpeg a WAV goes straight in; anything else can't.
	dir = toolsDir(t, "", "whisper-cli")
	os.MkdirAll(filepath.Join(dir, "models"), 0o755)
	os.WriteFile(filepath.Join(dir, "models", "ggml-tiny.bin"), []byte("model"), 0o600)
	r = toolRegistry(dir)
	env, _, _, err = runTool(t, r, "audio.transcribe", map[string]string{"model": "tiny"}, map[string][]byte{"memo.wav": []byte("RIFF....WAVE")}, "memo.wav")
	if err != nil {
		t.Fatal(err)
	}
	if got := runs(calls(t, dir)); len(got) != 1 || got[0][4] != filepath.Join(env.Dir, "memo.wav") || got[0][6] != "auto" {
		t.Fatalf("calls without ffmpeg %q", got)
	}
	if _, _, _, err := runTool(t, r, "audio.transcribe", map[string]string{"model": "tiny"}, map[string][]byte{"memo.mp3": []byte("x")}, "memo.mp3"); err == nil || !strings.Contains(err.Error(), "no ffmpeg") {
		t.Fatalf("mp3 without ffmpeg: %v", err)
	}
}

func TestDocTextReadsTheTextLayerOrRecognizesPages(t *testing.T) {
	dir := toolsDir(t, "", "pdftotext", "pdftoppm", "tesseract")
	r := toolRegistry(dir)

	// A PDF with text: pdftotext alone.
	env, out, _, err := runTool(t, r, "doc.text", nil, map[string][]byte{"-letter.pdf": []byte("%PDF TEXT:Dear neighbour, the hedge is lovely.")}, "-letter.pdf")
	if err != nil {
		t.Fatal(err)
	}
	got := runs(calls(t, dir))
	if len(got) != 1 || !slices.Equal(got[0], []string{"pdftotext", "-enc", "UTF-8", filepath.Join(env.Dir, "-letter.pdf"), got[0][4]}) ||
		filepath.Dir(filepath.Dir(got[0][4])) != env.Dir {
		t.Fatalf("calls %q", got)
	}
	if b, _ := os.ReadFile(env.out(0)); string(b) != "Dear neighbour, the hedge is lovely.\f" || !strings.Contains(out, "the text of 1 page") {
		t.Fatalf("text %q, stdout %q", b, out)
	}

	// A scan (no text layer), 12 pages: rendered, then each recognized in
	// page order (pdftoppm pads 1 to 01).
	before := len(calls(t, dir))
	env, out, _, err = runTool(t, r, "doc.text", map[string]string{"language": "eng+deu"}, map[string][]byte{"scan.pdf": []byte("%PDF" + strings.Repeat(" PAGE", 12))}, "scan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	got = calls(t, dir)[before:]
	if len(got) != 14 || got[0][0] != "pdftotext" || got[1][0] != "pdftoppm" {
		t.Fatalf("calls %q", got)
	}
	work := filepath.Dir(got[0][4])
	if !slices.Equal(got[1], []string{"pdftoppm", "-png", "-gray", "-scale-to", "3500", "-l", "100", filepath.Join(env.Dir, "scan.pdf"), filepath.Join(work, "page")}) {
		t.Fatalf("pdftoppm argv %q", got[1])
	}
	for i, c := range got[2:] {
		want := []string{"tesseract", filepath.Join(work, fmt.Sprintf("page-%02d.png", i+1)), filepath.Join(work, fmt.Sprintf("ocr-%04d", i+1)), "-l", "eng+deu"}
		if !slices.Equal(c, want) {
			t.Fatalf("tesseract call %d:\n got %q\nwant %q", i+1, c, want)
		}
	}
	text, _ := os.ReadFile(env.out(0))
	pages := strings.Split(strings.TrimSuffix(string(text), "\f"), "\f")
	if len(pages) != 12 || pages[0] != "recognized page 1 (eng+deu)\n" || pages[11] != "recognized page 12 (eng+deu)\n" || !strings.Contains(out, "no text layer") {
		t.Fatalf("recognized text %q, stdout %q", text, out)
	}

	// method text keeps a scan's (empty) text layer; a picture has none.
	if _, _, _, err := runTool(t, r, "doc.text", map[string]string{"method": "text"}, map[string][]byte{"scan.pdf": []byte("PAGE")}, "scan.pdf"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runTool(t, r, "doc.text", map[string]string{"method": "text"}, map[string][]byte{"note.png": testPNG(8, 8)}, "note.png"); err == nil || !strings.Contains(err.Error(), "no text layer") {
		t.Fatalf("picture with method text: %v", err)
	}

	// A picture: tesseract straight away.
	before = len(calls(t, dir))
	env, _, _, err = runTool(t, r, "doc.text", map[string]string{"method": "ocr", "language": "fra"}, map[string][]byte{"note.png": testPNG(8, 8)}, "note.png")
	if err != nil {
		t.Fatal(err)
	}
	got = calls(t, dir)[before:]
	if len(got) != 1 || got[0][1] != filepath.Join(env.Dir, "note.png") || filepath.Base(got[0][2]) != "ocr" || got[0][4] != "fra" {
		t.Fatalf("calls %q", got)
	}
	if b, _ := os.ReadFile(env.out(0)); string(b) != "recognized note.png (fra)\n" {
		t.Fatalf("text %q", b)
	}

	// A picture over the pixel limit is refused from its header, before
	// tesseract sees it.
	bomb := testPNG(1, 1)
	binary.BigEndian.PutUint32(bomb[16:], 50000)
	binary.BigEndian.PutUint32(bomb[20:], 50000)
	binary.BigEndian.PutUint32(bomb[29:], crc32.ChecksumIEEE(bomb[12:29]))
	before = len(calls(t, dir))
	if _, _, _, err := runTool(t, r, "doc.text", map[string]string{"method": "ocr"}, map[string][]byte{"bomb.png": bomb}, "bomb.png"); err == nil || !strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("bomb: %v", err)
	}
	if len(calls(t, dir)) != before {
		t.Fatal("tesseract was given the bomb")
	}

	// Recognizing a PDF's pages needs pdftoppm.
	r = toolRegistry(toolsDir(t, "", "pdftotext", "tesseract"))
	if _, _, _, err := runTool(t, r, "doc.text", map[string]string{"method": "ocr"}, map[string][]byte{"scan.pdf": []byte("PAGE")}, "scan.pdf"); err == nil || !strings.Contains(err.Error(), "pdftoppm") {
		t.Fatalf("PDF OCR without pdftoppm: %v", err)
	}
}
