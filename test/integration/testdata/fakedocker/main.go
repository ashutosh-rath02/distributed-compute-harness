// A stand-in for the docker CLI, for the container tests. Every call is
// logged as a JSON line to calls.jsonl next to the binary, so a test can
// assert the exact command line. It keeps its "containers" as files in
// containers/ next to it:
//
//	version / info          "linux/amd64 99.0.0-fake" (the platform from a
//	                        file named platform, if there is one); fails
//	                        like a stopped daemon while daemon-down exists
//	ps -a --filter label=K=V --format ...   names of matching containers
//	kill NAME / rm -f NAME  marks it killed (its run exits 137)
//	run [--flag=value ...] IMAGE CMD ARGS...
//
// run honours the --mount flags it was given: container paths under a
// mount's target map to its source, anything else (or a readonly mount) is
// a read-only file system. CMD is one of:
//
//	copy SRC DST     copy a file, e.g. /in/data.txt to /out/result.txt
//	echo WORDS...    print them
//	sleep SECONDS    wait (until killed)
//	exit CODE        exit with CODE
//	fill DST BYTES   write BYTES to DST slowly (until killed)
//	symlink TARGET DST   make DST a symlink to TARGET (as given)
//
// A file named pull-delay (milliseconds) makes run wait that long before
// the container exists, like a pull.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var dir string

func main() {
	exe, _ := os.Executable()
	dir = filepath.Dir(exe)
	args := os.Args[1:]
	logCall(args)
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "version", "info":
		if exists(filepath.Join(dir, "daemon-down")) {
			fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon. Is the docker daemon running?")
			os.Exit(1)
		}
		platform := "linux/amd64"
		if b, err := os.ReadFile(filepath.Join(dir, "platform")); err == nil {
			platform = strings.TrimSpace(string(b))
		}
		fmt.Println(platform + " 99.0.0-fake")
	case "ps":
		ps(args[1:])
	case "kill":
		if len(args) != 2 || !killContainer(args[1]) {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", strings.Join(args[1:], " "))
			os.Exit(1)
		}
		fmt.Println(args[1])
	case "rm":
		code := 0
		for _, name := range args[1:] {
			if name == "-f" {
				continue
			}
			if !killContainer(name) {
				fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", name)
				code = 1
			}
			os.Remove(statePath(name))
		}
		os.Exit(code)
	case "run":
		os.Exit(run(args[1:]))
	default:
		fmt.Fprintf(os.Stderr, "fakedocker: unknown command %q\n", args[0])
		os.Exit(2)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func logCall(args []string) {
	b, _ := json.Marshal(map[string]any{"args": args})
	f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

type state struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

func statePath(name string) string  { return filepath.Join(dir, "containers", name+".json") }
func killedPath(name string) string { return filepath.Join(dir, "containers", name+".killed") }

func killContainer(name string) bool {
	if !exists(statePath(name)) {
		return false
	}
	os.WriteFile(killedPath(name), nil, 0o600)
	return true
}

func ps(args []string) {
	var key, value string
	for i := 0; i < len(args); i++ {
		if args[i] == "--filter" && i+1 < len(args) {
			kv := strings.TrimPrefix(args[i+1], "label=")
			key, value, _ = strings.Cut(kv, "=")
			i++
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "containers"))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var s state
		b, _ := os.ReadFile(filepath.Join(dir, "containers", e.Name()))
		if json.Unmarshal(b, &s) == nil && s.Labels[key] == value {
			fmt.Println(s.Name)
		}
	}
}

type mount struct {
	source, target string
	readonly       bool
}

func run(args []string) int {
	var name string
	var mounts []mount
	labels := map[string]string{}
	rm := false
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "--"); i++ {
		k, v, _ := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		switch k {
		case "rm":
			rm = true
		case "name":
			name = v
		case "label":
			lk, lv, _ := strings.Cut(v, "=")
			labels[lk] = lv
		case "mount":
			var m mount
			for _, f := range strings.Split(v, ",") {
				fk, fv, _ := strings.Cut(f, "=")
				switch fk {
				case "source":
					m.source = fv
				case "target":
					m.target = fv
				case "readonly":
					m.readonly = true
				}
			}
			mounts = append(mounts, m)
		}
	}
	if i >= len(args) {
		fmt.Fprintln(os.Stderr, "docker: 'docker run' requires at least 1 argument.")
		return 125
	}
	cmd := args[i+1:]
	if b, err := os.ReadFile(filepath.Join(dir, "pull-delay")); err == nil {
		ms, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		fmt.Fprintf(os.Stderr, "Unable to find image '%s' locally\n", args[i])
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	os.MkdirAll(filepath.Join(dir, "containers"), 0o700)
	b, _ := json.Marshal(state{Name: name, Labels: labels})
	os.WriteFile(statePath(name), b, 0o600)
	code := container(name, mounts, cmd)
	if rm {
		os.Remove(statePath(name))
	}
	return code
}

// resolve maps a container path to the host path its mount gives it.
func resolve(mounts []mount, p string, write bool) (string, error) {
	for _, m := range mounts {
		if p == m.target || strings.HasPrefix(p, m.target+"/") {
			if write && m.readonly {
				return "", fmt.Errorf("%s: Read-only file system", p)
			}
			return filepath.Join(m.source, filepath.FromSlash(strings.TrimPrefix(p, m.target))), nil
		}
	}
	if write {
		return "", fmt.Errorf("%s: Read-only file system", p)
	}
	return "", fmt.Errorf("%s: No such file or directory", p)
}

func killed(name string) bool { return exists(killedPath(name)) }

func container(name string, mounts []mount, cmd []string) int {
	defer os.Remove(killedPath(name))
	if len(cmd) == 0 {
		fmt.Println("hello from the image's own command")
		return 0
	}
	fail := func(err error) int { fmt.Fprintln(os.Stderr, cmd[0]+": "+err.Error()); return 1 }
	switch cmd[0] {
	case "copy":
		src, err := resolve(mounts, cmd[1], false)
		if err != nil {
			return fail(err)
		}
		dst, err := resolve(mounts, cmd[2], true)
		if err != nil {
			return fail(err)
		}
		in, err := os.Open(src)
		if err != nil {
			return fail(err)
		}
		defer in.Close()
		out, err := os.Create(dst)
		if err != nil {
			return fail(err)
		}
		io.Copy(out, in)
		out.Close()
		fmt.Printf("copied %s to %s\n", cmd[1], cmd[2])
	case "echo":
		fmt.Println(strings.Join(cmd[1:], " "))
	case "sleep":
		secs, _ := strconv.ParseFloat(cmd[1], 64)
		deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
		fmt.Println("sleeping")
		for time.Now().Before(deadline) {
			if killed(name) {
				return 137
			}
			time.Sleep(20 * time.Millisecond)
		}
	case "exit":
		code, _ := strconv.Atoi(cmd[1])
		fmt.Fprintf(os.Stderr, "exiting with %d\n", code)
		return code
	case "fill":
		dst, err := resolve(mounts, cmd[1], true)
		if err != nil {
			return fail(err)
		}
		total, _ := strconv.ParseInt(cmd[2], 10, 64)
		f, err := os.Create(dst)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		chunk := make([]byte, 64<<10)
		for written := int64(0); written < total; written += int64(len(chunk)) {
			if killed(name) {
				return 137
			}
			f.Write(chunk)
			time.Sleep(10 * time.Millisecond)
		}
	case "symlink":
		dst, err := resolve(mounts, cmd[2], true)
		if err != nil {
			return fail(err)
		}
		if err := os.Symlink(cmd[1], dst); err != nil {
			return fail(err)
		}
	default:
		fmt.Fprintf(os.Stderr, "exec: %q: executable file not found in $PATH\n", cmd[0])
		return 127
	}
	return 0
}
