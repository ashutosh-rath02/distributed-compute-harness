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
)

// Config holds the agent's tunables.
type Config struct {
	ManagerAddr       string
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
	conn, err := a.transport.Dial(ctx, a.cfg.ManagerAddr)
	if err != nil {
		return fmt.Errorf("dial manager: %w", err)
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

func (a *Agent) register(ctx context.Context, conn domain.Conn) error {
	manifest := a.buildManifest()
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

func (a *Agent) buildManifest() domain.Manifest {
	hostname, _ := os.Hostname()
	name := a.cfg.Name
	if name == "" {
		name = hostname
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
		// Resources/Capabilities are populated starting Milestone 6.
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
			payload := protocol.HeartbeatPayload{
				RuntimeState: domain.RuntimeState{
					NodeID:        a.identity.NodeID,
					State:         domain.NodeReady,
					LastHeartbeat: time.Now().UTC(),
					AgentVersion:  a.cfg.AgentVersion,
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
