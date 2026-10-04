package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
	"home-harness/internal/protocol"
	"home-harness/internal/tasks"
)

// Split sessions (roadmap item 10), device side: a model too big for one
// device runs with llama.cpp — ggml-rpc-server on each helper, llama-server
// on the main device with --rpc pointing at them. ggml-rpc-server has no
// authentication, so it listens on the helper's loopback only and the main
// device reaches it through the session's tunnels (tunnel.go). The
// binaries are the device owner's own install of llama.cpp (-llama-cpp-dir);
// the agent never downloads them.

type llamaCpp struct {
	dir     string
	once    sync.Once
	version string
}

// DefaultLlamaCppDir is where the agent looks for llama.cpp when
// -llama-cpp-dir isn't given: a "llama.cpp" folder next to the agent's
// own data (%LOCALAPPDATA%\HomeHarness\llama.cpp on Windows,
// ~/.home-harness/llama.cpp elsewhere).
func DefaultLlamaCppDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "HomeHarness", "llama.cpp")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".home-harness", "llama.cpp")
}

// bin is the path of one of llama.cpp's programs, or "" if it isn't there.
func (l *llamaCpp) bin(name string) string {
	if l == nil || l.dir == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(l.dir, name)
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		return ""
	}
	return p
}

var llamaVersionLine = regexp.MustCompile(`version:\s*(\S+)(?:\s*\((\w+)\))?`)

// buildVersion is the installed build ("11382-abc1234"), read once:
// every part of a session must run the same one.
func (l *llamaCpp) buildVersion(ctx context.Context) string {
	l.once.Do(func() {
		l.version = "unknown"
		bin := l.bin("llama-server")
		if bin == "" {
			bin = l.bin("ggml-rpc-server")
		}
		if bin == "" {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
		if m := llamaVersionLine.FindStringSubmatch(string(out)); m != nil {
			l.version = m[1]
			if m[2] != "" {
				l.version += "-" + m[2]
			}
		}
	})
	return l.version
}

// sweep stops llama.cpp programs still running from this
// agent's llama.cpp folder at start: only the agent starts them, so any
// found now were left by an agent that ended without stopping them.
func (l *llamaCpp) sweep() {
	if l == nil || l.dir == "" {
		return
	}
	targets := map[string]bool{}
	for _, name := range []string{"ggml-rpc-server", "llama-server"} {
		if p := l.bin(name); p != "" {
			targets[strings.ToLower(filepath.Clean(p))] = true
		}
	}
	if len(targets) == 0 {
		return
	}
	procs, err := process.Processes()
	if err != nil {
		return
	}
	for _, p := range procs {
		exe, err := p.Exe()
		if err == nil && targets[strings.ToLower(filepath.Clean(exe))] {
			log.Printf("agent: stopping %s (pid %d), left running by an earlier agent", exe, p.Pid)
			p.Kill()
		}
	}
}

// rememberTunnels keeps an assignment's tunnel tokens for its run.
func (a *Agent) rememberTunnels(wl domain.WorkloadID, grants []protocol.TunnelGrant) {
	if len(grants) == 0 {
		return
	}
	a.tun.mu.Lock()
	if a.tun.grants == nil {
		a.tun.grants = map[domain.WorkloadID][]protocol.TunnelGrant{}
	}
	a.tun.grants[wl] = grants
	a.tun.mu.Unlock()
}

func (a *Agent) forgetTunnels(wl domain.WorkloadID) {
	a.tun.mu.Lock()
	delete(a.tun.grants, wl)
	a.tun.mu.Unlock()
}

func (a *Agent) tunnelGrants(wl domain.WorkloadID) []protocol.TunnelGrant {
	a.tun.mu.Lock()
	defer a.tun.mu.Unlock()
	return append([]protocol.TunnelGrant(nil), a.tun.grants[wl]...)
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// startProgram starts cmd tied to the agent's life (childproc_*.go),
// returning a channel that receives its exit.
func startProgram(cmd *exec.Cmd) (<-chan error, error) {
	prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	adoptChild(cmd)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// waitUntil polls ready every 250 ms until it's true, the program exits,
// or ctx ends.
func waitUntil(ctx context.Context, exited <-chan error, what string, ready func() bool) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exited:
			return fmt.Errorf("%s exited before it was ready: %v", what, err)
		case <-tick.C:
		}
	}
	return nil
}

// ended turns a program's exit into the run's result: a cancel is the
// session ending, anything else is a failure worth reporting.
func ended(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return fmt.Errorf("%s stopped", what)
	}
	return fmt.Errorf("%s stopped: %w", what, err)
}

// ---- llm.split-helper

type splitHelper struct{ a *Agent }

func (h splitHelper) Available(context.Context) error {
	if h.a.llama.bin("ggml-rpc-server") == "" {
		return errors.New("llama.cpp's ggml-rpc-server isn't installed (-llama-cpp-dir)")
	}
	return nil
}

func (h splitHelper) Attributes(ctx context.Context) map[string]string {
	return map[string]string{catalog.AttrLlamaVersion: h.a.llama.buildVersion(ctx)}
}

// rpcCacheDir is where ggml-rpc-server keeps the model pieces it was
// sent, so the next session with the same model loads fast. It can hold
// gigabytes: its size is reported at the end of every session.
func rpcCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "HomeHarness", "llama-rpc-cache")
	}
	return filepath.Join(os.TempDir(), "HomeHarness-llama-rpc-cache")
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (h splitHelper) Run(ctx context.Context, env tasks.Env) error {
	session := env.Params["session"]
	index, _ := strconv.Atoi(env.Params["index"])
	var token string
	for _, g := range h.a.tunnelGrants(env.Workload) {
		if g.Session == session && g.Index == index {
			token = g.Token
		}
	}
	if token == "" {
		return errors.New("the manager sent no tunnel for this split session")
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return err
	}
	cache := rpcCacheDir()
	os.MkdirAll(cache, 0o700)
	cmd := exec.CommandContext(ctx, h.a.llama.bin("ggml-rpc-server"), "-H", "127.0.0.1", "-p", strconv.Itoa(port), "-c")
	cmd.Env = append(os.Environ(), "LLAMA_CACHE="+cache, "GGML_RPC_NO_RDMA=1")
	cmd.Stdout, cmd.Stderr = env.Stdout, env.Stderr
	exited, err := startProgram(cmd)
	if err != nil {
		return fmt.Errorf("start ggml-rpc-server: %w", err)
	}
	defer func() {
		fmt.Fprintf(env.Stderr, "model cache on this device: %d MB in %s\n", dirSize(cache)>>20, cache)
	}()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := waitUntil(ctx, exited, "ggml-rpc-server", func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}); err != nil {
		return err
	}
	withdraw := h.a.ExposeService(session, index, port, token)
	defer withdraw()
	fmt.Fprintf(env.Stdout, "%s: helper %d on this device's loopback port %d\n", catalog.SplitHelperReady, index, port)
	select {
	case err := <-exited:
		return ended(ctx, "ggml-rpc-server", err)
	case <-ctx.Done():
		<-exited
		return ctx.Err()
	}
}

// ---- llm.split-main

type splitMain struct{ a *Agent }

func (m splitMain) Available(context.Context) error {
	if m.a.llama.bin("llama-server") == "" {
		return errors.New("llama.cpp's llama-server isn't installed (-llama-cpp-dir)")
	}
	return nil
}

func (m splitMain) Attributes(ctx context.Context) map[string]string {
	return map[string]string{catalog.AttrLlamaVersion: m.a.llama.buildVersion(ctx)}
}

func (m splitMain) Run(ctx context.Context, env tasks.Env) error {
	session, model, name := env.Params["session"], env.Params["model"], env.Params["name"]
	helpers, _ := strconv.Atoi(env.Params["helpers"])
	if fi, err := os.Stat(model); err != nil || fi.IsDir() {
		return fmt.Errorf("model file %s isn't on this device", model)
	}
	var grants []protocol.TunnelGrant
	for _, g := range m.a.tunnelGrants(env.Workload) {
		if g.Session == session {
			grants = append(grants, g)
		}
	}
	if len(grants) != helpers {
		return fmt.Errorf("the manager sent %d tunnels for %d helpers", len(grants), helpers)
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].Index < grants[j].Index })
	var rpc []string
	for _, g := range grants {
		addr, err := m.a.TunnelListen(ctx, g.Token)
		if err != nil {
			return fmt.Errorf("tunnel to helper %d: %w", g.Index, err)
		}
		rpc = append(rpc, addr)
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, m.a.llama.bin("llama-server"), "-m", model, "--rpc", strings.Join(rpc, ","),
		"-ngl", "999", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--alias", name)
	cmd.Env = append(os.Environ(), "GGML_RPC_NO_RDMA=1")
	cmd.Stdout, cmd.Stderr = env.Stdout, env.Stderr
	exited, err := startProgram(cmd)
	if err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// Loading sends the helpers their share of the model: minutes, for a
	// big one over Wi-Fi.
	if err := waitUntil(ctx, exited, "llama-server", func() bool {
		resp, err := http.Get(base + "/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}); err != nil {
		return err
	}
	withdraw := m.a.handlers.ServeModel(name, base)
	m.a.requestReprobe()
	defer func() {
		withdraw()
		m.a.requestReprobe()
	}()
	fmt.Fprintf(env.Stdout, "%s: %s split over this device and %d helper(s)\n", catalog.SplitMainReady, name, helpers)
	select {
	case err := <-exited:
		return ended(ctx, "llama-server", err)
	case <-ctx.Done():
		<-exited
		return ctx.Err()
	}
}
