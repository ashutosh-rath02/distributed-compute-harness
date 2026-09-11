// Package agent implements the node-side lifecycle: load/generate
// identity, connect to the manager, register, heartbeat, and respond to
// protocol messages. It depends on domain and protocol, never on a
// concrete transport package.
package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/protocol"
	"home-harness/internal/sysinfo"
)

// metricsSampleInterval is how long CollectMetrics blocks per heartbeat to
// sample CPU usage. It is independent of HeartbeatInterval, which governs
// how often that sample is taken and sent.
const metricsSampleInterval = 200 * time.Millisecond

// Config holds the agent's tunables.
type Config struct {
	// ManagerAddr, if set, is used directly and Discoverer is never
	// consulted — the manual "--manager-addr" fallback from v1.md §4.1
	// for networks where discovery doesn't work.
	ManagerAddr string
	// Discoverer resolves a manager address when ManagerAddr is empty. It
	// is consulted on every (re)connect attempt, not just once, so the
	// agent tolerates the manager's address changing across restarts —
	// churn is normal (baseline §9 rule 4).
	Discoverer   domain.Discoverer
	PairingToken string
	IdentityDir  string
	Name         string
	AgentVersion string

	HeartbeatInterval time.Duration
	// ReconnectBackoff is both the starting delay after a failed
	// connection attempt and the delay reset to after a connection that
	// stayed up for a while — it doubles (capped at MaxReconnectBackoff)
	// on each consecutive failure, so a persistently unreachable manager
	// is retried less aggressively over time rather than hammering it.
	ReconnectBackoff time.Duration
	// MaxReconnectBackoff caps the doubling. Defaults to 30s.
	MaxReconnectBackoff time.Duration
	// InsecureWorkloadsDisabled, when true, makes the agent refuse every
	// WORKLOAD_ASSIGN instead of running it. Set when the transport is
	// plaintext with no manager authentication (-insecure): over such a
	// connection the agent cannot tell a legitimate manager from a rogue
	// one on the same network, and a workload is arbitrary code execution
	// — a much worse thing to hand to an unauthenticated peer than the
	// harmless v0 command set.
	InsecureWorkloadsDisabled bool
	// LaunchArgs is the exact argv (os.Args[1:]) this process was started
	// with, captured once in cmd/agent/main.go. A self-update (see
	// selfupdate.go) execs the new binary with these same args rather than
	// trying to reconstruct or guess at any flag — including
	// -manager-fingerprint and -insecure below, which aren't otherwise
	// threaded into Config at all since nothing but the relaunch needs them
	// as values (main() only ever needed them to build the transport).
	LaunchArgs []string
	// Insecure mirrors the -insecure flag: whether this process is running
	// with a plaintext, unauthenticated transport. selfupdate.go uses it to
	// decide http:// vs https:// (and whether ManagerFingerprint below is
	// even meaningful) for its own binary download.
	Insecure bool
	// ManagerFingerprint is the -manager-fingerprint flag's value (empty
	// when Insecure). selfupdate.go builds its download client's TLS trust
	// from this via the same mtls.PinnedClientConfig cmd/agent/main.go
	// already calls to build the main WS transport — not a new trust
	// mechanism, just reused for a second connection.
	ManagerFingerprint string
}

const defaultAgentVersion = "0.1.0"

// Agent is a single node's runtime: identity plus the connection lifecycle
// to the manager.
type Agent struct {
	cfg       Config
	transport domain.Transport
	identity  *identity.Identity
	startedAt time.Time
	executor  *Executor
	// binaryHash is this process's own executable's SHA-256, computed once
	// in New — see selfupdate.go's hashFile. Empty if it couldn't be
	// computed (e.g. os.Executable() failing on an unusual platform);
	// reported as-is, an empty BinaryHash just means the manager can never
	// consider this node up to date, never a crash.
	binaryHash string

	// addrMu guards currentManagerAddr, set by connectAndServe and read by
	// a SELF_UPDATE command handler running on a different goroutine (the
	// receive loop) — the only field on Agent that's written after
	// construction from more than one goroutine.
	addrMu             sync.Mutex
	currentManagerAddr string
}

// New loads (or generates, on first run) the agent's identity and returns
// a ready-to-Run agent.
func New(transport domain.Transport, cfg Config) (*Agent, error) {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.ReconnectBackoff == 0 {
		cfg.ReconnectBackoff = 3 * time.Second
	}
	if cfg.MaxReconnectBackoff == 0 {
		cfg.MaxReconnectBackoff = 30 * time.Second
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = defaultAgentVersion
	}

	id, err := identity.LoadOrCreate(cfg.IdentityDir)
	if err != nil {
		return nil, fmt.Errorf("agent: load identity: %w", err)
	}

	binaryHash := ""
	if exePath, err := os.Executable(); err != nil {
		log.Printf("agent %s: could not determine own executable path, self-update unavailable: %v", id.NodeID, err)
	} else {
		removeStaleUpdateFile(exePath)
		if h, err := hashFile(exePath); err != nil {
			log.Printf("agent %s: could not hash own executable, self-update unavailable: %v", id.NodeID, err)
		} else {
			binaryHash = h
		}
	}

	return &Agent{cfg: cfg, transport: transport, identity: id, startedAt: time.Now(), executor: NewExecutor(), binaryHash: binaryHash}, nil
}

// NodeID returns this agent's persistent node identity.
func (a *Agent) NodeID() domain.NodeID { return a.identity.NodeID }

func (a *Agent) setCurrentManagerAddr(addr string) {
	a.addrMu.Lock()
	a.currentManagerAddr = addr
	a.addrMu.Unlock()
}

func (a *Agent) getCurrentManagerAddr() string {
	a.addrMu.Lock()
	defer a.addrMu.Unlock()
	return a.currentManagerAddr
}

// Run connects to the manager and services the connection until ctx is
// canceled, reconnecting with exponential backoff on failure (v1.md §16
// "Reconnect after network failure"). A connection that stayed up longer
// than a few heartbeat intervals counts as a real session and resets the
// backoff to its starting value; a rapid failure loop keeps doubling it
// (capped at MaxReconnectBackoff) instead of hammering an unreachable
// manager at a fixed rate forever.
func (a *Agent) Run(ctx context.Context) error {
	backoff := a.cfg.ReconnectBackoff
	resetThreshold := 3 * a.cfg.HeartbeatInterval

	for {
		connectedAt := time.Now()
		if err := a.connectAndServe(ctx); err != nil {
			log.Printf("agent %s: connection error: %v", a.identity.NodeID, err)
		}
		// The manager marks this node's in-flight workloads FAILED as soon
		// as it notices the disconnect (failWorkloadsFor) — keeping a
		// workload running past that point would leave the manager and
		// agent permanently disagreeing about whether it's still going.
		a.executor.CancelCurrent()

		if time.Since(connectedAt) >= resetThreshold {
			backoff = a.cfg.ReconnectBackoff
		} else {
			backoff = nextBackoff(backoff, a.cfg.MaxReconnectBackoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// nextBackoff doubles current, capped at maxBackoff. A pure function so
// the backoff progression is testable without any real timing.
func nextBackoff(current, maxBackoff time.Duration) time.Duration {
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

func (a *Agent) connectAndServe(ctx context.Context) error {
	addr, err := a.resolveManagerAddr(ctx)
	if err != nil {
		return fmt.Errorf("resolve manager address: %w", err)
	}
	a.setCurrentManagerAddr(addr)

	conn, err := a.transport.Dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("dial manager at %s: %w", addr, err)
	}
	defer conn.Close()

	if err := a.register(ctx, conn); err != nil {
		return err
	}
	log.Printf("agent %s: registered with manager", a.identity.NodeID)

	errCh := make(chan error, 2)
	go a.heartbeatLoop(ctx, conn, errCh)
	go a.receiveLoop(ctx, conn, errCh)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) resolveManagerAddr(ctx context.Context) (string, error) {
	if a.cfg.ManagerAddr != "" {
		return a.cfg.ManagerAddr, nil
	}
	if a.cfg.Discoverer == nil {
		return "", fmt.Errorf("no ManagerAddr configured and no Discoverer set")
	}
	return a.cfg.Discoverer.Discover(ctx)
}

func (a *Agent) register(ctx context.Context, conn domain.Conn) error {
	manifest := a.buildManifest(ctx)
	signature := a.identity.Sign(protocol.RegisterSignedData(a.cfg.PairingToken, a.identity.NodeID))
	if err := a.send(ctx, conn, protocol.MsgRegister, domain.ManagerNodeID,
		protocol.RegisterPayload{Manifest: manifest, PairingToken: a.cfg.PairingToken, Signature: signature}); err != nil {
		return fmt.Errorf("send REGISTER: %w", err)
	}

	env, err := a.receiveEnvelope(ctx, conn)
	if err != nil {
		return fmt.Errorf("await REGISTER_ACK: %w", err)
	}

	switch env.Type {
	case protocol.MsgRegisterAck:
		return nil
	case protocol.MsgRegisterReject:
		var payload protocol.RegisterRejectPayload
		_ = env.DecodePayload(&payload)
		return fmt.Errorf("registration rejected: %s", payload.Reason)
	default:
		return fmt.Errorf("unexpected message type during registration: %s", env.Type)
	}
}

func (a *Agent) buildManifest(ctx context.Context) domain.Manifest {
	hostname, _ := os.Hostname()
	name := a.cfg.Name
	if name == "" {
		name = hostname
	}

	resources, capabilities, err := sysinfo.Manifest(ctx)
	if err != nil {
		// Resource/capability reporting is best-effort: a node that can't
		// introspect its own hardware should still be able to register and
		// heartbeat, rather than being unable to join the fabric at all.
		log.Printf("agent %s: collecting resources/capabilities: %v", a.identity.NodeID, err)
	}

	return domain.Manifest{
		SchemaVersion: domain.ManifestSchemaVersion,
		Node: domain.Node{
			Identity:     a.identity.Identity,
			Hostname:     hostname,
			Name:         name,
			Platform:     domain.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH},
			AgentVersion: a.cfg.AgentVersion,
			BinaryHash:   a.binaryHash,
		},
		Resources:    resources,
		Capabilities: capabilities,
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context, conn domain.Conn, errCh chan<- error) {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics, err := sysinfo.CollectMetrics(ctx, metricsSampleInterval)
			if err != nil {
				log.Printf("agent %s: collecting metrics: %v", a.identity.NodeID, err)
			}
			payload := protocol.HeartbeatPayload{
				RuntimeState: domain.RuntimeState{
					NodeID:               a.identity.NodeID,
					State:                domain.NodeReady,
					CPUPercent:           metrics.CPUPercent,
					MemoryAvailableBytes: metrics.MemoryAvailableBytes,
					LastHeartbeat:        time.Now().UTC(),
					AgentVersion:         a.cfg.AgentVersion,
				},
			}
			if err := a.send(ctx, conn, protocol.MsgHeartbeat, domain.ManagerNodeID, payload); err != nil {
				errCh <- fmt.Errorf("send HEARTBEAT: %w", err)
				return
			}
		}
	}
}

func (a *Agent) receiveLoop(ctx context.Context, conn domain.Conn, errCh chan<- error) {
	for {
		env, err := a.receiveEnvelope(ctx, conn)
		if err != nil {
			errCh <- err
			return
		}
		switch env.Type {
		case protocol.MsgPing:
			if err := a.send(ctx, conn, protocol.MsgPong, env.Source, nil); err != nil {
				errCh <- fmt.Errorf("send PONG: %w", err)
				return
			}
		case protocol.MsgCommand:
			a.handleCommand(ctx, conn, env, errCh)
		case protocol.MsgWorkloadAssign:
			a.handleWorkloadAssign(ctx, conn, env)
		case protocol.MsgWorkloadCancel:
			a.handleWorkloadCancel(ctx, conn, env)
		case protocol.MsgError:
			var payload protocol.ErrorPayload
			_ = env.DecodePayload(&payload)
			log.Printf("agent %s: received ERROR from manager: %s: %s", a.identity.NodeID, payload.Code, payload.Message)
		default:
			log.Printf("agent %s: unhandled message type %s", a.identity.NodeID, env.Type)
		}
	}
}

func (a *Agent) receiveEnvelope(ctx context.Context, conn domain.Conn) (*protocol.Envelope, error) {
	data, err := conn.Receive(ctx)
	if err != nil {
		return nil, err
	}
	env, err := protocol.Decode(data)
	if err != nil {
		return nil, err
	}
	if err := protocol.CheckVersion(env); err != nil {
		return nil, err
	}
	return env, nil
}

func (a *Agent) send(ctx context.Context, conn domain.Conn, msgType protocol.MessageType, dest domain.NodeID, payload any) error {
	env, err := protocol.NewEnvelope(msgType, a.identity.NodeID, dest, payload)
	if err != nil {
		return err
	}
	wire, err := protocol.Encode(env)
	if err != nil {
		return err
	}
	return conn.Send(ctx, wire)
}
