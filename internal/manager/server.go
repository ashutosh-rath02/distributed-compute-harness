package manager

import (
	"context"
	"log"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
)

// Config holds the manager's tunables.
type Config struct {
	Addr             string
	PairingToken     string
	HeartbeatTimeout time.Duration
}

// Server is the control-plane process: it accepts connections over a
// domain.Transport, runs the REGISTER/HEARTBEAT protocol, and owns the
// node registry.
type Server struct {
	cfg       Config
	transport domain.Transport
	Registry  *Registry
}

// NewServer builds a manager bound to a concrete transport (e.g. the ws
// package) but coded only against domain.Transport.
func NewServer(transport domain.Transport, cfg Config) *Server {
	return &Server{
		cfg:       cfg,
		transport: transport,
		Registry:  NewRegistry(),
	}
}

// Run starts accepting connections and monitoring heartbeats until ctx is
// canceled.
func (s *Server) Run(ctx context.Context) error {
	conns, err := s.transport.Listen(ctx, s.cfg.Addr)
	if err != nil {
		return err
	}

	go s.monitorHeartbeats(ctx)

	for {
		select {
		case conn, ok := <-conns:
			if !ok {
				return nil
			}
			go s.handleConn(ctx, conn)
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *Server) monitorHeartbeats(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.HeartbeatTimeout / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, id := range s.Registry.ExpireStale(s.cfg.HeartbeatTimeout) {
				log.Printf("node.offline: %s", id)
			}
		}
	}
}

func (s *Server) handleConn(ctx context.Context, conn domain.Conn) {
	var nodeID domain.NodeID
	defer func() {
		if nodeID != "" {
			s.Registry.SetState(nodeID, domain.NodeOffline)
			log.Printf("node.offline: %s (connection closed)", nodeID)
		}
		conn.Close()
	}()

	for {
		data, err := conn.Receive(ctx)
		if err != nil {
			return
		}
		env, err := protocol.Decode(data)
		if err != nil {
			log.Printf("manager: decode error from %s: %v", conn.RemoteAddr(), err)
			continue
		}
		if err := protocol.CheckVersion(env); err != nil {
			s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "UNSUPPORTED_VERSION", err.Error())
			continue
		}

		switch env.Type {
		case protocol.MsgRegister:
			nodeID = s.handleRegister(ctx, conn, env)
		case protocol.MsgHeartbeat:
			s.handleHeartbeat(env)
		case protocol.MsgPing:
			s.handlePing(ctx, conn, env)
		default:
			s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "UNSUPPORTED_TYPE", string(env.Type))
		}
	}
}

func (s *Server) handleRegister(ctx context.Context, conn domain.Conn, env *protocol.Envelope) domain.NodeID {
	var payload protocol.RegisterPayload
	if err := env.DecodePayload(&payload); err != nil {
		s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "BAD_PAYLOAD", err.Error())
		return ""
	}

	if payload.PairingToken != s.cfg.PairingToken {
		s.send(ctx, conn, protocol.MsgRegisterReject, domain.ManagerNodeID, env.Source,
			protocol.RegisterRejectPayload{Reason: "invalid pairing token"})
		log.Printf("manager: rejected registration from %s: bad pairing token", conn.RemoteAddr())
		return ""
	}

	node := payload.Manifest.Node
	rec, isNew := s.Registry.Upsert(node, conn)
	s.Registry.SetState(node.Identity.NodeID, domain.NodeReady)

	if isNew {
		log.Printf("node.registered: %s (%s)", node.Identity.NodeID, node.Name)
	} else {
		log.Printf("node.reconnected: %s (%s)", node.Identity.NodeID, node.Name)
	}
	_ = rec

	s.send(ctx, conn, protocol.MsgRegisterAck, domain.ManagerNodeID, node.Identity.NodeID,
		protocol.RegisterAckPayload{NodeID: node.Identity.NodeID, ServerTime: time.Now().UTC()})

	return node.Identity.NodeID
}

func (s *Server) handleHeartbeat(env *protocol.Envelope) {
	var payload protocol.HeartbeatPayload
	if err := env.DecodePayload(&payload); err != nil {
		log.Printf("manager: bad heartbeat payload from %s: %v", env.Source, err)
		return
	}
	s.Registry.Touch(env.Source)
}

func (s *Server) handlePing(ctx context.Context, conn domain.Conn, env *protocol.Envelope) {
	s.send(ctx, conn, protocol.MsgPong, domain.ManagerNodeID, env.Source, nil)
}

func (s *Server) sendError(ctx context.Context, conn domain.Conn, source, dest domain.NodeID, code, message string) {
	s.send(ctx, conn, protocol.MsgError, source, dest, protocol.ErrorPayload{Code: code, Message: message})
}

func (s *Server) send(ctx context.Context, conn domain.Conn, msgType protocol.MessageType, source, dest domain.NodeID, payload any) {
	env, err := protocol.NewEnvelope(msgType, source, dest, payload)
	if err != nil {
		log.Printf("manager: build envelope: %v", err)
		return
	}
	wire, err := protocol.Encode(env)
	if err != nil {
		log.Printf("manager: encode envelope: %v", err)
		return
	}
	if err := conn.Send(ctx, wire); err != nil {
		log.Printf("manager: send to %s: %v", conn.RemoteAddr(), err)
	}
}
