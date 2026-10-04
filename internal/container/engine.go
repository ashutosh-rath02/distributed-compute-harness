// Package container runs container.run tasks (roadmap item 13) with the
// device's own container engine: the docker CLI, or podman where docker
// isn't installed (Docker Desktop on Windows and macOS, Docker Engine or
// Podman on Linux). The agent never installs one. The engine is found by
// absolute path only — a bare-name lookup crashes the agent on Android
// (HANDOVER §11) — and offered only while its daemon answers and runs
// Linux containers, since the hardening below assumes them.
//
// Every container runs with a read-only root filesystem, no capabilities,
// no new privileges, a process limit, a non-root user, CPU and memory
// limits and no network unless the task asks for one (which policy must
// allow). The only mounts are the workload's own files: its inputs at
// /in, read-only, and an empty /out whose named files come back. The
// task's arguments go after the image, so they are only ever the
// container's own command line.
package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"home-harness/internal/catalog"
)

// Engine is a container engine's command-line program.
type Engine struct {
	Path string // absolute
	Kind string // "docker" or "podman"
}

// Labels on every container an agent starts: the sweep at start finds
// its own leftovers by them.
const (
	LabelOwner    = "home-harness.agent"
	LabelWorkload = "home-harness.workload"
)

// PidsLimit caps the processes in one container (a fork bomb stays
// inside it).
const PidsLimit = 256

// Find locates the engine program for setting: a program, a folder
// holding docker or podman, "auto" for the standard install locations
// (docker first), or "" / "off" for none. Only absolute paths are used,
// and only os.Stat touches them.
func Find(setting string) (Engine, bool) {
	var candidates []string
	switch setting {
	case "", "off":
		return Engine{}, false
	case "auto":
		candidates = standardLocations()
	default:
		fi, err := os.Stat(setting)
		if err != nil {
			return Engine{}, false
		}
		if !fi.IsDir() {
			candidates = []string{setting}
			break
		}
		for _, name := range []string{"docker", "podman"} {
			candidates = append(candidates, filepath.Join(setting, name+exeSuffix()))
		}
	}
	for _, c := range candidates {
		if !filepath.IsAbs(c) {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() {
			kind := "docker"
			if strings.Contains(strings.ToLower(filepath.Base(c)), "podman") {
				kind = "podman"
			}
			return Engine{Path: c, Kind: kind}, true
		}
	}
	return Engine{}, false
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// standardLocations is where Docker Desktop, Docker Engine and Podman put
// their command-line programs on this OS, docker's first.
func standardLocations() []string {
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return []string{
			filepath.Join(pf, "Docker", "Docker", "resources", "bin", "docker.exe"),
			filepath.Join(pf, "RedHat", "Podman", "podman.exe"),
		}
	case "darwin":
		var out []string
		home, _ := os.UserHomeDir()
		if home != "" {
			out = append(out, filepath.Join(home, ".docker", "bin", "docker"))
		}
		return append(out,
			"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker",
			"/opt/homebrew/bin/podman", "/usr/local/bin/podman", "/opt/podman/bin/podman")
	default:
		return []string{"/usr/bin/docker", "/usr/local/bin/docker", "/snap/bin/docker", "/usr/bin/podman", "/usr/local/bin/podman"}
	}
}

// command runs the engine program with args. The program's own folder
// goes first on PATH, so the helpers installed next to it (Docker
// Desktop's credential helpers) are found. Once ctx ends, a program
// that won't let go of its output (a helper it started still holding
// it) is waited for only briefly.
func (e Engine) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, e.Path, args...)
	cmd.Env = withPathDir(os.Environ(), filepath.Dir(e.Path))
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func withPathDir(env []string, dir string) []string {
	out := append([]string(nil), env...)
	for i, kv := range out {
		k, v, _ := strings.Cut(kv, "=")
		if k == "PATH" || (runtime.GOOS == "windows" && strings.EqualFold(k, "PATH")) {
			out[i] = k + "=" + dir + string(os.PathListSeparator) + v
			return out
		}
	}
	return append(out, "PATH="+dir)
}

// Info is what a probe learned about the engine.
type Info struct {
	Engine   Engine
	Version  string // the daemon's ("27.1.1")
	Platform string // what its containers run as ("linux/amd64")
}

var (
	platformRe = regexp.MustCompile(`^(?:` + catalog.PlatformPattern + `)$`)
	versionRe  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_~-]{0,63}$`)
)

// probe asks the engine about its daemon: an error means it doesn't
// answer (Docker Desktop not started) or can't run Linux containers.
// Podman has no daemon on Linux, so "podman info" (which reaches its
// machine VM on Windows and macOS) stands in for "docker version".
func (e Engine) probe(ctx context.Context) (Info, error) {
	args := []string{"version", "--format", "{{.Server.Os}}/{{.Server.Arch}} {{.Server.Version}}"}
	if e.Kind == "podman" {
		args = []string{"info", "--format", "{{.Host.OS}}/{{.Host.Arch}} {{.Version.Version}}"}
	}
	out, err := e.command(ctx, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg := strings.TrimSpace(string(ee.Stderr))
			if len(msg) > 200 {
				msg = msg[:200] + "..."
			}
			return Info{}, fmt.Errorf("%s isn't answering: %s", e.Kind, msg)
		}
		return Info{}, fmt.Errorf("%s isn't answering: %v", e.Kind, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || !platformRe.MatchString(fields[0]) {
		return Info{}, fmt.Errorf("%s answered %q, not its platform and version", e.Kind, strings.TrimSpace(string(out)))
	}
	if goos, _, _ := strings.Cut(fields[0], "/"); goos != "linux" {
		return Info{}, fmt.Errorf("%s runs %s containers; container tasks need Linux containers (in Docker Desktop: Switch to Linux containers)", e.Kind, goos)
	}
	version := fields[1]
	if !versionRe.MatchString(version) {
		version = "unknown"
	}
	return Info{Engine: e, Version: version, Platform: fields[0]}, nil
}

// Spec is one container run.
type Spec struct {
	Name     string // unique per attempt: what cancel kills
	Owner    string // the agent's node ID (LabelOwner)
	Workload string
	Image    string   // canonical, registry explicit
	Args     []string // the container's command line, after the image
	CPUs     string   // canonical number ("1", "0.5")
	MemoryMB int
	Network  string // "none" or "bridge"
	Platform string // "" = the engine's own
	User     string // "uid:gid", never root
	KeepID   bool   // podman on Linux: map the agent's user into the container
	// InDir and OutDir are the host folders mounted at /in (read-only) and
	// /out; "" = not mounted.
	InDir, OutDir string
}

// RunArgs is the engine's command line for s (after the program name).
// Nothing a task supplies reaches it except validated numbers, the
// enum-checked network, a pattern-checked platform and canonical image,
// and the container's own arguments after the image.
func (e Engine) RunArgs(s Spec) []string {
	a := []string{"run", "--rm", "--name=" + s.Name,
		"--label=" + LabelOwner + "=" + s.Owner, "--label=" + LabelWorkload + "=" + s.Workload,
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=" + strconv.Itoa(PidsLimit),
		"--user=" + s.User}
	if s.KeepID {
		a = append(a, "--userns=keep-id")
	}
	if e.Kind == "podman" {
		// An image's declared VOLUMEs would otherwise become writable
		// storage on the device; Docker has no switch for this.
		a = append(a, "--image-volume=ignore")
	}
	network := "none"
	if s.Network == "bridge" {
		network = "bridge"
	}
	a = append(a, "--network="+network, "--cpus="+s.CPUs,
		fmt.Sprintf("--memory=%dm", s.MemoryMB), fmt.Sprintf("--memory-swap=%dm", s.MemoryMB), // no swap beyond the limit
		"--log-driver=none") // output comes through the attached CLI; nothing piles up in the engine's logs
	if s.Platform != "" {
		a = append(a, "--platform="+s.Platform)
	}
	if s.InDir != "" {
		a = append(a, "--mount=type=bind,source="+s.InDir+",target=/in,readonly")
	}
	if s.OutDir != "" {
		a = append(a, "--mount=type=bind,source="+s.OutDir+",target=/out")
	}
	a = append(a, s.Image)
	return append(a, s.Args...)
}

// runAs is the container's user: the agent's own on Unix (so its files in
// /out stay the agent's to read and delete; podman on Linux maps it with
// keep-id, since rootless podman shifts user IDs), and "nobody" on
// Windows or when the agent itself runs as root.
func runAs(kind string) (user string, keepID bool) {
	uid, gid := os.Getuid(), os.Getgid()
	if runtime.GOOS == "windows" || uid <= 0 {
		return "65534:65534", false
	}
	return fmt.Sprintf("%d:%d", uid, gid), kind == "podman" && runtime.GOOS == "linux"
}
