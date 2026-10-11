// Command loadsim measures how the manager behaves with many devices. It
// starts a throwaway manager (a real manager.exe, own folder, own ports),
// connects N simulated agents that speak the real protocol (signed
// REGISTER, heartbeats, workload assignments answered after a short
// simulated run), submits work at a steady rate, and reports registration
// time, protocol round trips, queue-wait and submission latency
// percentiles, throughput, the manager's CPU and memory, database growth
// and what a few dashboard event streams cost.
//
// The agents are deliberately light (no system probing), so what is
// measured is the manager, not the simulator. Run it from a second
// terminal-sized budget: 500 agents is a few hundred MB for the manager
// and the simulator together on this machine.
//
//	go build -o loadsim.exe ./cmd/loadsim
//	loadsim -manager path\to\manager.exe -agents 200 -rate 50 -duration 60s
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/mtls"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

type options struct {
	manager   string
	agents    int
	duration  time.Duration
	rate      float64
	workMS    int
	slots     int
	heartbeat time.Duration
	port      int
	sse       int
	dir       string
	ramp      time.Duration
	jobTasks  int
	restart   bool
}

func main() {
	var o options
	flag.StringVar(&o.manager, "manager", "", "path to manager.exe (required)")
	flag.IntVar(&o.agents, "agents", 50, "simulated agents")
	flag.DurationVar(&o.duration, "duration", 30*time.Second, "how long to submit work")
	flag.Float64Var(&o.rate, "rate", 20, "workloads submitted per second (spread over -job-tasks sized jobs when > 0)")
	flag.IntVar(&o.workMS, "work-ms", 100, "simulated run time of each workload")
	flag.IntVar(&o.slots, "slots", 4, "workload slots each simulated agent offers")
	flag.DurationVar(&o.heartbeat, "heartbeat", 5*time.Second, "heartbeat interval of the simulated agents (the real agent's default)")
	flag.IntVar(&o.port, "port", 18130, "first of two loopback ports (agents, API); use 18120-18139")
	flag.IntVar(&o.sse, "sse", 3, "dashboard event streams to hold open")
	flag.StringVar(&o.dir, "dir", "", "working folder (default a fresh one under the temp folder)")
	flag.DurationVar(&o.ramp, "ramp", 0, "spread agent connections over this long (0 = all at once, the worst case)")
	flag.IntVar(&o.jobTasks, "job-tasks", 1, "tasks per submission: 1 submits single workloads, more submits jobs of that many tasks")
	flag.BoolVar(&o.restart, "restart", false, "after the run, kill the manager and time how long the fleet takes to reconnect")
	flag.Parse()
	if o.manager == "" {
		fmt.Fprintln(os.Stderr, "loadsim: -manager is required")
		os.Exit(2)
	}
	if o.port < 18120 || o.port > 18138 {
		fmt.Fprintln(os.Stderr, "loadsim: -port must be within 18120-18138")
		os.Exit(2)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "loadsim:", err)
		os.Exit(1)
	}
}

// ---- the manager under test

type mgrProc struct {
	cmd         *exec.Cmd
	pid         int
	fingerprint string
	token       string
	apiBase     string
	agentAddr   string
	dbPath      string
}

var fpRe = regexp.MustCompile(`^\s+([0-9a-f]{64})\s*$`)

func startManager(o options, dir string) (*mgrProc, error) {
	m := &mgrProc{
		agentAddr: fmt.Sprintf("127.0.0.1:%d", o.port),
		apiBase:   fmt.Sprintf("http://127.0.0.1:%d", o.port+1),
		dbPath:    filepath.Join(dir, "manager.db"),
	}
	args := []string{"-addr", m.agentAddr, "-api-addr", fmt.Sprintf("127.0.0.1:%d", o.port+1), "-disable-discovery", "-keep-awake=false",
		"-allow-raw-commands", "-pairing-token", "loadsim-pairing-token", "-heartbeat-timeout", "20s",
		"-operator-token-file", filepath.Join(dir, "operator-token"), "-db", m.dbPath, "-tls-dir", filepath.Join(dir, "tls"),
		"-artifact-dir", filepath.Join(dir, "artifacts")}
	m.cmd = exec.Command(o.manager, args...)
	stderr, err := m.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	m.cmd.Stdout = io.Discard
	logf, _ := os.Create(filepath.Join(dir, "manager.log"))
	if err := m.cmd.Start(); err != nil {
		return nil, err
	}
	m.pid = m.cmd.Process.Pid
	fpCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if logf != nil {
				fmt.Fprintln(logf, line)
			}
			if mm := fpRe.FindStringSubmatch(line); mm != nil {
				select {
				case fpCh <- mm[1]:
				default:
				}
			}
		}
	}()
	select {
	case m.fingerprint = <-fpCh:
	case <-time.After(20 * time.Second):
		m.stop()
		return nil, fmt.Errorf("the manager printed no fingerprint in 20s (see %s)", filepath.Join(dir, "manager.log"))
	}
	for i := 0; i < 80; i++ {
		b, err := os.ReadFile(filepath.Join(dir, "operator-token"))
		if err == nil {
			m.token = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i := 0; i < 80; i++ {
		if resp, err := http.Get(m.apiBase + "/server-proof?nonce=aa"); err == nil {
			resp.Body.Close()
			return m, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	m.stop()
	return nil, fmt.Errorf("the manager API never came up")
}

func (m *mgrProc) stop() {
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Kill()
		m.cmd.Wait()
	}
}

func (m *mgrProc) api(method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, m.apiBase+path, rd)
	req.Header.Set("Authorization", "Bearer "+m.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// ---- stats

type samples struct {
	mu sync.Mutex
	v  []float64
}

func (s *samples) add(d time.Duration) {
	s.mu.Lock()
	s.v = append(s.v, float64(d)/float64(time.Millisecond))
	s.mu.Unlock()
}

func (s *samples) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.v) == 0 {
		return "no samples"
	}
	v := append([]float64{}, s.v...)
	sort.Float64s(v)
	p := func(q float64) float64 { return v[min(len(v)-1, int(q*float64(len(v))))] }
	return fmt.Sprintf("n=%d p50=%.1fms p90=%.1fms p99=%.1fms max=%.1fms", len(v), p(.5), p(.9), p(.99), v[len(v)-1])
}

// ---- a simulated agent

type simAgent struct {
	id        *identity.Identity
	name      string
	o         options
	transport *ws.Transport
	addr      string

	registeredAt atomic.Int64 // unix nanos of the last REGISTER_ACK
	connected    atomic.Bool
}

type fleet struct {
	o      options
	m      *mgrProc
	agents []*simAgent

	regTime, pingRTT, queueWait, submitLat samples
	registered, regFailed                  atomic.Int64
	assigned, completed                    atomic.Int64
	submitted, submitErrs                  atomic.Int64
	sseEvents                              atomic.Int64
	reconnectDone                          atomic.Int64
	errMu                                  sync.Mutex
	errSeen                                map[string]int
}

// noteErr counts connection failures by their first 90 characters, so the
// report can say what went wrong instead of only how often.
func (f *fleet) noteErr(err error) {
	s := err.Error()
	if len(s) > 90 {
		s = s[:90]
	}
	f.errMu.Lock()
	if f.errSeen == nil {
		f.errSeen = map[string]int{}
	}
	f.errSeen[s]++
	f.errMu.Unlock()
}

func newAgent(o options, m *mgrProc, i int) (*simAgent, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	id := &identity.Identity{Identity: domain.Identity{NodeID: identity.DeriveNodeID(pub), PublicKey: pub}, PrivateKey: priv}
	return &simAgent{id: id, name: fmt.Sprintf("sim-%04d", i), o: o,
		transport: ws.NewTLSClient(mtls.PinnedClientConfig(m.fingerprint)), addr: m.agentAddr}, nil
}

func (a *simAgent) send(ctx context.Context, c domain.Conn, t protocol.MessageType, payload any) error {
	env, err := protocol.NewEnvelope(t, a.id.NodeID, domain.ManagerNodeID, payload)
	if err != nil {
		return err
	}
	wire, err := protocol.Encode(env)
	if err != nil {
		return err
	}
	return c.Send(ctx, wire)
}

func (a *simAgent) manifest() domain.Manifest {
	return domain.Manifest{
		SchemaVersion: domain.ManifestSchemaVersion,
		Node: domain.Node{Identity: a.id.Identity, Hostname: a.name, Name: a.name,
			Platform: domain.Platform{OS: "linux", Architecture: "amd64"}, AgentVersion: "loadsim"},
		Resources:     []domain.Resource{{Kind: domain.ResourceCPUCores, Capacity: 8, Unit: "cores"}, {Kind: domain.ResourceMemoryBytes, Capacity: 16 << 30, Unit: "bytes"}},
		Capabilities:  []domain.Capability{{Name: domain.CapabilitySystemExecute, Version: "1"}},
		WorkloadSlots: a.o.slots,
	}
}

// serve connects, registers and then services the connection until it ends
// or ctx does. It returns after a connection ends so the caller can
// reconnect.
func (f *fleet) serve(ctx context.Context, a *simAgent) error {
	start := time.Now()
	conn, err := a.transport.Dial(ctx, a.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	use := domain.DeviceUse{}
	reg := protocol.RegisterPayload{Manifest: a.manifest(), PairingToken: "loadsim-pairing-token",
		Signature: a.id.Sign(protocol.RegisterSignedData("loadsim-pairing-token", a.id.NodeID)), Use: &use}
	if err := a.send(ctx, conn, protocol.MsgRegister, reg); err != nil {
		return err
	}
	data, err := conn.Receive(ctx)
	if err != nil {
		return err
	}
	env, err := protocol.Decode(data)
	if err != nil || env.Type != protocol.MsgRegisterAck {
		return fmt.Errorf("registration not acknowledged")
	}
	f.regTime.add(time.Since(start))
	a.registeredAt.Store(time.Now().UnixNano())
	a.connected.Store(true)
	f.registered.Add(1)
	defer a.connected.Store(false)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var pingAt atomic.Int64
	go func() { // heartbeats, and a protocol round trip once per beat
		t := time.NewTicker(a.o.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				hb := protocol.HeartbeatPayload{RuntimeState: domain.RuntimeState{NodeID: a.id.NodeID, State: domain.NodeReady,
					CPUPercent: 5, MemoryAvailableBytes: 8 << 30, LastHeartbeat: time.Now().UTC(), AgentVersion: "loadsim", Use: &use}}
				if a.send(cctx, conn, protocol.MsgHeartbeat, hb) != nil {
					return
				}
				pingAt.Store(time.Now().UnixNano())
				if a.send(cctx, conn, protocol.MsgPing, nil) != nil {
					return
				}
			}
		}
	}()
	for {
		data, err := conn.Receive(cctx)
		if err != nil {
			return err
		}
		env, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.MsgPong:
			if t := pingAt.Swap(0); t != 0 {
				f.pingRTT.add(time.Since(time.Unix(0, t)))
			}
		case protocol.MsgPing:
			a.send(cctx, conn, protocol.MsgPong, nil)
		case protocol.MsgWorkloadAssign:
			var p protocol.WorkloadAssignPayload
			if env.DecodePayload(&p) != nil {
				continue
			}
			f.assigned.Add(1)
			for _, arg := range p.Workload.Args {
				if ns, ok := strings.CutPrefix(arg, "t="); ok {
					if n, err := strconv.ParseInt(ns, 10, 64); err == nil {
						f.queueWait.add(time.Since(time.Unix(0, n)))
					}
				}
			}
			go func(w domain.Workload) {
				st := domain.WorkloadStatus{ID: w.ID, Target: a.id.NodeID, State: domain.WorkloadRunning, StartedAt: time.Now().UTC()}
				a.send(cctx, conn, protocol.MsgWorkloadStatus, protocol.WorkloadStatusPayload{Status: st})
				time.Sleep(time.Duration(a.o.workMS) * time.Millisecond)
				st.State, st.FinishedAt, st.Stdout = domain.WorkloadCompleted, time.Now().UTC(), "ok\n"
				if a.send(cctx, conn, protocol.MsgWorkloadStatus, protocol.WorkloadStatusPayload{Status: st}) == nil {
					f.completed.Add(1)
				}
			}(p.Workload)
		case protocol.MsgWorkloadCancel:
		}
	}
}

// keepConnected runs one agent for the whole test, reconnecting with the
// same jitter the real agent uses.
func (f *fleet) keepConnected(ctx context.Context, a *simAgent, first bool) {
	backoff := 3 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := f.serve(ctx, a)
		if err != nil && ctx.Err() == nil && !a.connected.Load() && time.Since(began) < 30*time.Second {
			f.regFailed.Add(1)
			f.noteErr(err)
		}
		if time.Since(began) > 15*time.Second {
			backoff = 3 * time.Second
		}
		wait := backoff/2 + time.Duration(randInt63n(int64(backoff)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func randInt63n(n int64) int64 {
	var b [8]byte
	rand.Read(b[:])
	return int64(uint64(b[0])<<56|uint64(b[1])<<48|uint64(b[2])<<40|uint64(b[3])<<32|uint64(b[4])<<24|uint64(b[5])<<16|uint64(b[6])<<8|uint64(b[7])) & 0x7fffffffffffffff % n
}

// ---- driving the manager

func (f *fleet) submitLoop(ctx context.Context) {
	if f.o.rate <= 0 {
		return
	}
	interval := time.Duration(float64(time.Second) / f.o.rate * float64(f.o.jobTasks))
	t := time.NewTicker(interval)
	defer t.Stop()
	sem := make(chan struct{}, 64)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		select {
		case sem <- struct{}{}:
		default:
			f.submitErrs.Add(1) // the simulator itself can't keep up with the manager's answers
			continue
		}
		go func() {
			defer func() { <-sem }()
			begin := time.Now()
			stamp := "t=" + strconv.FormatInt(begin.UnixNano(), 10)
			var resp *http.Response
			var err error
			if f.o.jobTasks <= 1 {
				resp, err = f.m.api("POST", "/workloads", map[string]any{"command": "loadsim", "args": []string{stamp}})
			} else {
				tasks := make([]map[string]any, f.o.jobTasks)
				for i := range tasks {
					tasks[i] = map[string]any{"command": "loadsim", "args": []string{stamp, strconv.Itoa(i)}}
				}
				resp, err = f.m.api("POST", "/jobs", map[string]any{"tasks": tasks})
			}
			if err != nil {
				f.submitErrs.Add(1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				f.submitErrs.Add(1)
				return
			}
			f.submitLat.add(time.Since(begin))
			f.submitted.Add(int64(max(1, f.o.jobTasks)))
		}()
	}
}

func (f *fleet) sseReader(ctx context.Context) {
	req, _ := http.NewRequestWithContext(ctx, "GET", f.m.apiBase+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.m.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			f.sseEvents.Add(1)
		}
	}
}

type resourceTrack struct {
	mu               sync.Mutex
	cpuSum, cpuMax   float64
	n                int
	rssMax           uint64
	rssStart, rssEnd uint64
	threadsMax       int32
}

func trackProcess(ctx context.Context, pid int, rt *resourceTrack) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return
	}
	p.Percent(0)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cpu, _ := p.Percent(0)
		mi, err := p.MemoryInfo()
		if err != nil {
			return
		}
		th, _ := p.NumThreads()
		rt.mu.Lock()
		rt.cpuSum += cpu
		rt.n++
		rt.cpuMax = max(rt.cpuMax, cpu)
		rt.rssMax = max(rt.rssMax, mi.RSS)
		rt.rssEnd = mi.RSS
		if rt.rssStart == 0 {
			rt.rssStart = mi.RSS
		}
		rt.threadsMax = max(rt.threadsMax, th)
		rt.mu.Unlock()
	}
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func mib(b uint64) string { return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20)) }

func (f *fleet) waitRegistered(n int, limit time.Duration) time.Duration {
	start := time.Now()
	for time.Since(start) < limit {
		if int(f.registered.Load()) >= n {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

func run(o options) error {
	dir := o.dir
	if dir == "" {
		d, err := os.MkdirTemp("", "loadsim-")
		if err != nil {
			return err
		}
		dir = d
	}
	os.MkdirAll(dir, 0o700)
	m, err := startManager(o, dir)
	if err != nil {
		return err
	}
	defer m.stop()
	fmt.Printf("manager pid %d, folder %s\n", m.pid, dir)

	f := &fleet{o: o, m: m}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rt resourceTrack
	go trackProcess(ctx, m.pid, &rt)
	dbStart := fileSize(m.dbPath)

	for i := 0; i < o.agents; i++ {
		a, err := newAgent(o, m, i)
		if err != nil {
			return err
		}
		f.agents = append(f.agents, a)
	}
	regStart := time.Now()
	for i, a := range f.agents {
		if o.ramp > 0 {
			time.Sleep(o.ramp / time.Duration(o.agents))
		}
		_ = i
		go f.keepConnected(ctx, a, true)
	}
	allReg := f.waitRegistered(o.agents, 3*time.Minute)
	fmt.Printf("registered %d/%d agents in %.1fs (failed attempts: %d)\n", f.registered.Load(), o.agents, time.Since(regStart).Seconds(), f.regFailed.Load())
	_ = allReg

	for i := 0; i < o.sse; i++ {
		go f.sseReader(ctx)
	}
	time.Sleep(2 * time.Second)
	runCtx, stopRun := context.WithTimeout(ctx, o.duration)
	runStart := time.Now()
	completedAtStart := f.completed.Load()
	go f.submitLoop(runCtx)
	<-runCtx.Done()
	stopRun()
	elapsed := time.Since(runStart)
	// Let the queue drain, bounded.
	drainStart := time.Now()
	for time.Since(drainStart) < 30*time.Second && f.completed.Load() < f.submitted.Load() {
		time.Sleep(200 * time.Millisecond)
	}
	done := f.completed.Load() - completedAtStart

	rt.mu.Lock()
	avgCPU := 0.0
	if rt.n > 0 {
		avgCPU = rt.cpuSum / float64(rt.n)
	}
	rep := map[string]any{"agents": o.agents, "registered": f.registered.Load()}
	rt.mu.Unlock()
	_ = rep

	fmt.Println("---- results")
	f.errMu.Lock()
	for e, n := range f.errSeen {
		fmt.Printf("connection failure x%d: %s\n", n, e)
	}
	f.errMu.Unlock()
	fmt.Printf("agents: %d registered of %d (first full registration took %.1fs)\n", f.registered.Load(), o.agents, allReg.Seconds())
	fmt.Printf("registration time per agent (dial to ack): %s\n", f.regTime.summary())
	fmt.Printf("protocol round trip (PING->PONG under load): %s\n", f.pingRTT.summary())
	fmt.Printf("queue wait (submit to assignment at the device): %s\n", f.queueWait.summary())
	fmt.Printf("submission call latency: %s\n", f.submitLat.summary())
	fmt.Printf("submitted %d, assigned %d, completed %d in %.1fs; throughput %.1f tasks/s; submit errors/skipped %d\n",
		f.submitted.Load(), f.assigned.Load(), done, elapsed.Seconds(), float64(done)/elapsed.Seconds(), f.submitErrs.Load())
	rt.mu.Lock()
	fmt.Printf("manager CPU: avg %.0f%% max %.0f%% of one core; memory: start %s, peak %s, end %s; threads max %d\n",
		avgCPU, rt.cpuMax, mib(rt.rssStart), mib(rt.rssMax), mib(rt.rssEnd), rt.threadsMax)
	rt.mu.Unlock()
	fmt.Printf("store: %d -> %d bytes (+%d)\n", dbStart, fileSize(m.dbPath), fileSize(m.dbPath)-dbStart)
	fmt.Printf("event streams: %d held open, %d events received (%.0f/s each)\n", o.sse, f.sseEvents.Load(), float64(f.sseEvents.Load())/elapsed.Seconds()/float64(max(1, o.sse)))
	if resp, err := m.api("GET", "/nodes", nil); err == nil {
		var nodes []struct{ State string }
		json.NewDecoder(resp.Body).Decode(&nodes)
		resp.Body.Close()
		ready := 0
		for _, n := range nodes {
			if n.State == "READY" {
				ready++
			}
		}
		fmt.Printf("manager sees %d nodes, %d READY\n", len(nodes), ready)
	}

	if o.restart {
		// The thundering herd: kill the manager, start it again on the
		// same state, and time the fleet's return.
		before := f.registered.Load()
		m.stop()
		time.Sleep(time.Second)
		m2, err := startManager(options{manager: o.manager, port: o.port}, dir)
		if err != nil {
			return fmt.Errorf("restart: %w", err)
		}
		defer m2.stop()
		t0 := time.Now()
		f.m = m2
		fmt.Printf("manager restarted (same fingerprint: %v)\n", m2.fingerprint == m.fingerprint)
		for time.Since(t0) < 3*time.Minute && f.registered.Load() < before+int64(o.agents) {
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("fleet back: %d/%d reconnected within %.1fs\n", f.registered.Load()-before, o.agents, time.Since(t0).Seconds())
	}
	return nil
}
