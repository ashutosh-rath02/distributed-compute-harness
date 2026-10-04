package tasks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// External programs for the home media and document types (media.go,
// transcribe.go, doctext.go): ffmpeg, whisper.cpp's whisper-cli, poppler's
// pdftotext/pdftoppm and tesseract — the device owner's own installs, never
// downloaded by the agent. A program is found by checking files, never by
// exec.LookPath (on Android that lookup kills the whole agent: HANDOVER
// §11), and always run by its absolute path, with arguments the handler
// builds (no shell) naming only files inside the workload's directory, by
// absolute path so that no input name can ever read as an option.

// tools finds and runs those programs.
type tools struct {
	// dir is the device owner's tools folder (-tools-dir); "" = none.
	// Programs may sit in it, in its bin, or one folder down (an unpacked
	// release); whisper.cpp's models are in its models folder.
	dir string
	// system: also look in standard install locations and PATH.
	system bool
	run    func(*exec.Cmd) error

	mu     sync.Mutex
	probes map[string]toolProbe // path -> what -version said, while the file is unchanged
}

type toolProbe struct {
	size    int64
	mod     time.Time
	ok      bool
	version string
}

func newTools(opts Options) *tools {
	run := opts.RunProgram
	if run == nil {
		run = (*exec.Cmd).Run
	}
	return &tools{dir: opts.ToolsDir, system: opts.ToolsSearchSystem, run: run, probes: map[string]toolProbe{}}
}

// toolDirs are the folders searched for programs, in order.
func (t *tools) toolDirs() []string {
	var dirs []string
	if t.dir != "" {
		dirs = append(dirs, t.dir, filepath.Join(t.dir, "bin"))
		if entries, err := os.ReadDir(t.dir); err == nil {
			var subs []string
			for _, e := range entries {
				if e.IsDir() && e.Name() != "bin" && e.Name() != "models" {
					subs = append(subs, e.Name())
				}
			}
			sort.Strings(subs)
			for _, s := range subs {
				// An unpacked release: ffmpeg-7.1-essentials_build/bin,
				// whisper-bin-x64/Release, poppler-24.08.0/Library/bin.
				base := filepath.Join(t.dir, s)
				dirs = append(dirs, base, filepath.Join(base, "bin"), filepath.Join(base, "Release"), filepath.Join(base, "Library", "bin"))
			}
		}
	}
	if t.system {
		dirs = append(dirs, standardToolDirs()...)
		dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	}
	return dirs
}

// standardToolDirs are where package managers and installers put these
// programs — searched even when the agent's PATH is minimal (launchd,
// a scheduled task).
func standardToolDirs() []string {
	switch runtime.GOOS {
	case "windows":
		var dirs []string
		add := func(env string, rel ...string) {
			if base := os.Getenv(env); base != "" {
				dirs = append(dirs, filepath.Join(append([]string{base}, rel...)...))
			}
		}
		add("ProgramFiles", "ffmpeg", "bin")
		add("ProgramFiles", "Tesseract-OCR")
		add("LOCALAPPDATA", "Programs", "Tesseract-OCR")
		add("LOCALAPPDATA", "Microsoft", "WinGet", "Links")
		add("USERPROFILE", "scoop", "shims")
		add("ProgramData", "chocolatey", "bin")
		return dirs
	case "darwin":
		return []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin", "/usr/bin"}
	default:
		return []string{"/usr/bin", "/usr/local/bin", "/data/data/com.termux/files/usr/bin", "/snap/bin"}
	}
}

// find returns the absolute path of the program name, or "".
func (t *tools) find(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, dir := range t.toolDirs() {
		if dir == "" || !filepath.IsAbs(dir) {
			continue // a relative PATH entry would depend on the agent's working directory
		}
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || (runtime.GOOS != "windows" && fi.Mode()&0o111 == 0) {
			continue
		}
		return p
	}
	return ""
}

// probe runs `path -version` once per version of the file and reports
// whether it answered as want (the start of its first line), and the rest
// of that line (its version).
func (t *tools) probe(ctx context.Context, path, want string) (bool, string) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, ""
	}
	t.mu.Lock()
	p, ok := t.probes[path]
	t.mu.Unlock()
	if ok && p.size == fi.Size() && p.mod.Equal(fi.ModTime()) {
		return p.ok, p.version
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	line, _, _ := strings.Cut(string(out), "\n")
	line = strings.TrimSpace(line)
	p = toolProbe{size: fi.Size(), mod: fi.ModTime(), ok: err == nil && strings.HasPrefix(line, want)}
	if p.ok {
		p.version, _, _ = strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, want)), " ")
	}
	if ctx.Err() == nil { // a probe cut short says nothing about the program
		t.mu.Lock()
		t.probes[path] = p
		t.mu.Unlock()
	}
	return p.ok, p.version
}

// ffmpeg is the path of an ffmpeg that answers -version, or "".
func (t *tools) ffmpeg(ctx context.Context) (string, string) {
	p := t.find("ffmpeg")
	if p == "" {
		return "", ""
	}
	if ok, version := t.probe(ctx, p, "ffmpeg version"); ok {
		return p, version
	}
	return "", ""
}

// exec runs a program in the workload's directory, its output going to
// the task's. A failure says which program and how it ended, with the
// last thing it wrote to stderr.
func (t *tools) exec(ctx context.Context, env Env, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = env.Dir
	var tail tailBuffer
	cmd.Stdout = env.Stdout
	cmd.Stderr = io.MultiWriter(&tail, env.Stderr)
	err := t.run(cmd)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	name := strings.TrimSuffix(filepath.Base(path), ".exe")
	if last := tail.last(); last != "" {
		return fmt.Errorf("%s failed (%v): %s", name, err, last)
	}
	return fmt.Errorf("%s failed: %w", name, err)
}

// tailBuffer keeps the last 4 KiB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

// last is the last non-empty line written.
func (t *tailBuffer) last() string {
	lines := bytes.Split(bytes.TrimSpace(t.b), []byte("\n"))
	return strings.TrimSpace(string(lines[len(lines)-1]))
}

// tempDir is a fresh folder inside the workload's directory for a tool's
// intermediate files: a new name, so it can't be one of the inputs.
func tempDir(env Env) (string, error) {
	return os.MkdirTemp(env.Dir, "work-")
}
