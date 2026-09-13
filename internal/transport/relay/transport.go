// Package relay implements domain.Transport for devices that are not on
// the manager's LAN, by tunneling the exact same WebSocket protocol
// internal/transport/ws uses through a small self-hosted rendezvous relay
// (internal/relay) instead of a direct TCP dial/listen. Manager and node
// logic never import this package by concrete type — same as ws — so
// nothing above the transport layer needs to know a connection arrived via
// a relay at all.
package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"nhooyr.io/websocket"

	"home-harness/internal/domain"
	relayproto "home-harness/internal/relay"
	"home-harness/internal/transport/ws"
)

// Transport is the relay-backed implementation of domain.Transport. A
// given value is built for exactly one side: New/NewTLSServer for the
// manager (Listen), NewTLSClient/NewClient for the agent (Dial) — mirroring
// internal/transport/ws's New/NewTLSServer/NewTLSClient split.
type Transport struct {
	session   string
	ws        *ws.Transport // set for the Listen (manager) side only
	clientTLS *tls.Config   // set for the Dial (agent) side only; nil means the relay hop itself carries plaintext WS
}

// New returns a plaintext relay transport for the manager side (Listen).
func New(session string) *Transport {
	return &Transport{session: session, ws: ws.New()}
}

// NewTLSServer returns a relay transport for the manager side (Listen)
// that serves wss:// over the relay-provided connections using cert —
// the same certificate the manager's direct LAN listener uses, so a node
// connecting via the relay authenticates the manager exactly the same way
// as one connecting directly.
func NewTLSServer(session string, cert tls.Certificate) *Transport {
	return &Transport{session: session, ws: ws.NewTLSServer(cert)}
}

// NewClient returns a plaintext relay transport for the agent side (Dial).
func NewClient(session string) *Transport {
	return &Transport{session: session}
}

// NewTLSClient returns a relay transport for the agent side (Dial) that
// authenticates the manager over the relay hop using cfg (typically
// mtls.PinnedClientConfig's result) — identical trust model to a direct
// LAN connection, since the relay itself never terminates this TLS.
func NewTLSClient(session string, cfg *tls.Config) *Transport {
	return &Transport{session: session, clientTLS: cfg}
}

// Listen registers as the relay-listening side of t.session at addr (the
// relay server's address) and serves WebSocket upgrades over each pairing,
// via ws.Transport.ListenOn — see listener.go for how a pool of relay
// registrations is adapted into a net.Listener.
func (t *Transport) Listen(ctx context.Context, addr string) (<-chan domain.Conn, error) {
	if t.ws == nil {
		return nil, fmt.Errorf("relay: this Transport was constructed with NewClient/NewTLSClient, which can only Dial")
	}
	lis := newListener(addr, t.session)
	return t.ws.ListenOn(ctx, lis), nil
}

// Dial registers as the relay-connecting side of t.session at addr (the
// relay server's address), then runs the WebSocket client handshake over
// the resulting spliced connection — TLS first, if configured, exactly as
// a direct dial would, so the manager's fingerprint is verified end to end
// regardless of which path connected the two sides.
func (t *Transport) Dial(ctx context.Context, addr string) (domain.Conn, error) {
	raw, err := relayproto.DialConnect(ctx, addr, t.session)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}

	dialConn := net.Conn(raw)
	if t.clientTLS != nil {
		tlsConn := tls.Client(raw, t.clientTLS)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("relay: tls handshake: %w", err)
		}
		dialConn = tlsConn
	}

	// scheme is "ws" (not "wss") regardless of t.clientTLS: dialConn is
	// already the correctly-secured connection above when TLS is wanted,
	// and using "wss" here would make http.Transport additionally wrap it
	// in its own TLS client handshake on top of one we already did.
	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return dialConn, nil },
	}}
	wsConn, _, err := websocket.Dial(ctx, "ws://relay/harness", &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		dialConn.Close()
		return nil, fmt.Errorf("relay: ws handshake: %w", err)
	}
	return &conn{ws: wsConn, remote: raw.RemoteAddr().String()}, nil
}

// conn adapts a nhooyr.io/websocket connection to domain.Conn — the same
// small adapter internal/transport/ws's unexported conn type is, kept as
// an independent copy here rather than exported from ws for reuse: it's a
// few lines of pure boilerplate around a third-party type, not logic that
// would benefit from a shared abstraction.
type conn struct {
	ws     *websocket.Conn
	remote string
}

func (c *conn) Send(ctx context.Context, data []byte) error {
	if err := c.ws.Write(ctx, websocket.MessageBinary, data); err != nil {
		return fmt.Errorf("relay: send: %w", err)
	}
	return nil
}

func (c *conn) Receive(ctx context.Context) ([]byte, error) {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("relay: receive: %w", err)
	}
	return data, nil
}

func (c *conn) RemoteAddr() string { return c.remote }

func (c *conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "closed")
}
