package manager

import (
	"context"
	"fmt"
	"log"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/protocol"
)

// Config holds the manager's tunables.
type Config struct {
	Addr             string
	PairingToken     string
	HeartbeatTimeout time.Duration
}

// PersistentStore is the subset of persistent storage the manager needs:
// last-known node manifests that must survive a restart. The manager
// depends only on this interface, never on the concrete store package
// (e.g. internal/store/persistent), so storage can change later without
// touching manager logic.
type PersistentStore interface {
	UpsertNode(manifest domain.Manifest) error
	ListNodes() ([]domain.Manifest, error)
}

// Server is the control-plane process: it accepts connections over a
// domain.Transport, runs the REGISTER/HEARTBEAT protocol, and owns the
// node registry. Persistence is optional (nil store = registry only,
// nothing survives a restart) so tests and the walking-skeleton path from
// Milestone 3 keep working unchanged.
type Server struct {
	cfg       Config
	transport domain.Transport
	store     PersistentStore
	Registry  *Registry
}

// NewServer builds a manager bound to a concrete transport (e.g. the ws
// package) but coded only against domain.Transport, and an optional
// PersistentStore for node identity/metadata to survive a restart.
func NewServer(transport domain.Transport, store PersistentStore, cfg Config) *Server {
	return &Server{
		cfg:       cfg,
		transport: transport,
		store:     store,
		Registry:  NewRegistry(),
	}
}

// Run loads any previously-known nodes from the persistent store, then
// starts accepting connections and monitoring heartbeats until ctx is
// canceled.
func (s *Server) Run(ctx context.Context) error {
	if s.store != nil {
		manifests, err := s.store.ListNodes()
		if err != nil {
			return fmt.Errorf("manager: load persisted nodes: %w", err)
		}
		for _, m := range manifests {
			s.Registry.Seed(m.Node)
		}
	}

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
		if nodeID != "" && s.Registry.SetOfflineIfCurrent(nodeID, conn) {
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

		// Every message type except REGISTER requires this connection to
		// already be bound to a verified identity. Trusting env.Source
		// instead would let a registered connection forge messages on
		// behalf of a different node (baseline §5: network presence must
		// never imply execution authority — that extends to claimed
		// identity on an already-open connection, not just to admission).
		if env.Type != protocol.MsgRegister && nodeID == "" {
			s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "NOT_REGISTERED", "register before sending other messages")
			continue
		}

		switch env.Type {
		case protocol.MsgRegister:
			nodeID = s.handleRegister(ctx, conn, env)
		case protocol.MsgHeartbeat:
			s.handleHeartbeat(nodeID, env)
		case protocol.MsgPing:
			s.handlePing(ctx, conn, nodeID)
		default:
			s.sendError(ctx, conn, domain.ManagerNodeID, nodeID, "UNSUPPORTED_TYPE", string(env.Type))
		}
	}
}

func (s *Server) handleRegister(ctx context.Context, conn domain.Conn, env *protocol.Envelope) domain.NodeID {
	var payload protocol.RegisterPayload
	if err := env.DecodePayload(&payload); err != nil {
		s.sendError(ctx, conn, domain.ManagerNodeID, env.Source, "BAD_PAYLOAD", err.Error())
		return ""
	}

	node := payload.Manifest.Node
	claimedID := node.Identity.NodeID

	// Recompute the NodeID from the claimed public key rather than trusting
	// the claimed NodeID field as given — otherwise NodeID is just a
	// string with no cryptographic meaning.
	if identity.DeriveNodeID(node.Identity.PublicKey) != claimedID {
		s.reject(ctx, conn, env.Source, "identity mismatch: NodeID does not match public key")
		return ""
	}

	// Verify proof of possession of the private key for that public key.
	// Without this, knowing a valid pairing token would be enough to
	// register as ANY existing NodeID.
	signed := protocol.RegisterSignedData(payload.PairingToken, claimedID)
	if !identity.Verify(node.Identity, signed, payload.Signature) {
		s.reject(ctx, conn, env.Source, "invalid signature: proof of key possession failed")
		return ""
	}

	if payload.PairingToken != s.cfg.PairingToken {
		s.reject(ctx, conn, env.Source, "invalid pairing token")
		return ""
	}

	_, isNew := s.Registry.Upsert(node, conn)
	s.Registry.SetState(claimedID, domain.NodeReady)

	if s.store != nil {
		if err := s.store.UpsertNode(payload.Manifest); err != nil {
			log.Printf("manager: failed to persist node %s: %v", claimedID, err)
		}
	}

	if isNew {
		log.Printf("node.registered: %s (%s)", claimedID, node.Name)
	} else {
		log.Printf("node.reconnected: %s (%s)", claimedID, node.Name)
	}

	s.send(ctx, conn, protocol.MsgRegisterAck, domain.ManagerNodeID, claimedID,
		protocol.RegisterAckPayload{NodeID: claimedID, ServerTime: time.Now().UTC()})

	return claimedID
}

func (s *Server) reject(ctx context.Context, conn domain.Conn, dest domain.NodeID, reason string) {
	s.send(ctx, conn, protocol.MsgRegisterReject, domain.ManagerNodeID, dest,
		protocol.RegisterRejectPayload{Reason: reason})
	log.Printf("manager: rejected registration from %s: %s", conn.RemoteAddr(), reason)
}

func (s *Server) handleHeartbeat(nodeID domain.NodeID, env *protocol.Envelope) {
	var payload protocol.HeartbeatPayload
	if err := env.DecodePayload(&payload); err != nil {
		log.Printf("manager: bad heartbeat payload from %s: %v", nodeID, err)
		return
	}
	s.Registry.Touch(nodeID)
}

func (s *Server) handlePing(ctx context.Context, conn domain.Conn, nodeID domain.NodeID) {
	s.send(ctx, conn, protocol.MsgPong, domain.ManagerNodeID, nodeID, nil)
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
