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
	Discoverer        domain.Discoverer
	PairingToken      string
	IdentityDir       string
	Name              string
	AgentVersion      string
	HeartbeatInterval time.Duration
	ReconnectBackoff  time.Duration
}

const defaultAgentVersion = "0.1.0"

// Agent is a single node's runtime: identity plus the connection lifecycle
// to the manager.
type Agent struct {
	cfg       Config
	transport domain.Transport
	identity  *identity.Identity
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
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = defaultAgentVersion
	}

	id, err := identity.LoadOrCreate(cfg.IdentityDir)
	if err != nil {
		return nil, fmt.Errorf("agent: load identity: %w", err)
	}

	return &Agent{cfg: cfg, transport: transport, identity: id}, nil
}

// NodeID returns this agent's persistent node identity.
func (a *Agent) NodeID() domain.NodeID { return a.identity.NodeID }

// Run connects to the manager and services the connection until ctx is
// canceled, reconnecting with a fixed backoff on any failure (v1.md §16
// "Reconnect after network failure").
func (a *Agent) Run(ctx context.Context) error {
	for {
		if err := a.connectAndServe(ctx); err != nil {
			log.Printf("agent %s: connection error: %v", a.identity.NodeID, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.cfg.ReconnectBackoff):
		}
	}
}

func (a *Agent) connectAndServe(ctx context.Context) error {
	addr, err := a.resolveManagerAddr(ctx)
	if err != nil {
		return fmt.Errorf("resolve manager address: %w", err)
	}

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
