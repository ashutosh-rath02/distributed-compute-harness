// Command faketools stands in for the programs the media and document
// task types run, in tests: ffmpeg, whisper-cli, pdftotext, pdftoppm and
// tesseract, by the name it is run as. Each appends its argv to
// calls.jsonl next to itself, insists that every path it is given is
// absolute and every input exists, and writes small but plausible
// outputs, so a test can check both the exact arguments and the results.
// A file <name>.broken next to it makes that program fail (exit 1), even
// for -version.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	name := strings.TrimSuffix(filepath.Base(exe), ".exe")
	args := os.Args[1:]
	if f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		line, _ := json.Marshal(append([]string{name}, args...))
		f.Write(append(line, '\n'))
		f.Close()
	}
	if _, err := os.Stat(filepath.Join(dir, name+".broken")); err == nil {
		fail("%s: broken on purpose", name)
	}
	switch name {
	case "ffmpeg":
		ffmpeg(args)
	case "whisper-cli":
		whisper(args)
	case "pdftotext":
		pdftotext(args)
	case "pdftoppm":
		pdftoppm(args)
	case "tesseract":
		tesseract(args)
	default:
		fail("faketools: unknown program %q", name)
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// input reads a file the program was given: it must be an absolute path.
func input(p string) []byte {
	if !filepath.IsAbs(p) {
		fail("not an absolute path: %q", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		fail("can't read %s: %v", p, err)
	}
	return b
}

func output(p string, data []byte) {
	if !filepath.IsAbs(p) {
		fail("not an absolute path: %q", p)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		fail("can't write %s: %v", p, err)
	}
}

// ffmpeg: -version, or "... -i file:<in> ... -f <format> file:<out>".
func ffmpeg(args []string) {
	if len(args) == 1 && args[0] == "-version" {
		fmt.Println("ffmpeg version 7.1-fake Copyright (c) 2000-2024 the FFmpeg developers")
		fmt.Println("built with a test")
		return
	}
	var in, format string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-i":
			in = args[i+1]
		case "-f":
			format = args[i+1]
		}
	}
	out := args[len(args)-1]
	if !strings.HasPrefix(in, "file:") || !strings.HasPrefix(out, "file:") || format == "" {
		fail("ffmpeg: want -i file:<in> ... -f <format> file:<out>, got %q", args)
	}
	data := input(strings.TrimPrefix(in, "file:"))
	if format == "wav" {
		output(strings.TrimPrefix(out, "file:"), []byte("RIFF\x00\x00\x00\x00WAVE16k mono of "+filepath.Base(in)))
		return
	}
	output(strings.TrimPrefix(out, "file:"), []byte(fmt.Sprintf("%s made from %s (%d bytes)", format, filepath.Base(in), len(data))))
}

// whisper-cli -m <model> -f <wav> -l <lang> ... -of <base>: needs a WAV,
// like the real one.
func whisper(args []string) {
	flags := map[string]string{}
	set := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-m", "-f", "-l", "-t", "-of":
			if i+1 < len(args) {
				flags[args[i]] = args[i+1]
				i++
			}
		default:
			set[args[i]] = true
		}
	}
	input(flags["-m"])
	wav := input(flags["-f"])
	if !strings.HasPrefix(string(wav), "RIFF") {
		fail("whisper-cli: %s is not a WAV file", flags["-f"])
	}
	line := fmt.Sprintf("Hello from %s (%s, model %s)", filepath.Base(flags["-f"]), flags["-l"], filepath.Base(flags["-m"]))
	fmt.Printf("[00:00:00.000 --> 00:00:02.000]   %s\n", line)
	if set["-otxt"] {
		output(flags["-of"]+".txt", []byte(line+"\n"))
	}
	if set["-osrt"] {
		output(flags["-of"]+".srt", []byte("1\n00:00:00,000 --> 00:00:02,000\n"+line+"\n\n"))
	}
}

// pdftotext -enc UTF-8 <in> <out>: the text after "TEXT:" in the "PDF",
// or only page breaks for a "scan".
func pdftotext(args []string) {
	if len(args) != 4 || args[0] != "-enc" || args[1] != "UTF-8" {
		fail("pdftotext: unexpected arguments %q", args)
	}
	pdf := string(input(args[2]))
	text := "\f\f"
	if _, t, ok := strings.Cut(pdf, "TEXT:"); ok {
		text = t + "\f"
	}
	output(args[3], []byte(text))
}

// pdftoppm ... <in> <prefix>: one <prefix>-NN.png per "PAGE" in the
// "PDF", padded like the real one.
func pdftoppm(args []string) {
	if len(args) < 2 {
		fail("pdftoppm: unexpected arguments %q", args)
	}
	pdf := string(input(args[len(args)-2]))
	prefix := args[len(args)-1]
	pages := strings.Count(pdf, "PAGE")
	width := len(fmt.Sprint(pages))
	for i := 1; i <= pages; i++ {
		output(fmt.Sprintf("%s-%0*d.png", prefix, width, i), []byte(fmt.Sprintf("page %d", i)))
	}
}

// tesseract <image> <base> -l <lang>: writes <base>.txt.
func tesseract(args []string) {
	if len(args) != 4 || args[2] != "-l" {
		fail("tesseract: unexpected arguments %q", args)
	}
	img := input(args[0])
	what := filepath.Base(args[0])
	if strings.HasPrefix(string(img), "page ") {
		what = string(img)
	}
	output(args[1]+".txt", []byte(fmt.Sprintf("recognized %s (%s)\n", what, args[3])))
}
