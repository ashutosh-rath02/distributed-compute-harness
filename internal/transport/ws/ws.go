// Package ws implements domain.Transport over WebSocket-over-TCP. Node and
// manager logic never import this package directly by concrete type — they
// depend on domain.Transport/domain.Conn, so a future QUIC/mesh/relay
// transport can be swapped in without touching lifecycle or protocol code.
package ws

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"nhooyr.io/websocket"

	"home-harness/internal/domain"
)

// Transport is the WebSocket implementation of domain.Transport. Plain
// (New) is plaintext ws://, used unchanged by every existing test and by
// -insecure. TLS configuration is set at construction time by the
// composition root (cmd/manager, cmd/agent) via NewTLSServer/NewTLSClient
// — domain.Transport's shape never changes, so callers coded against the
// interface don't need to know which variant they got.
type Transport struct {
	serverTLS *tls.Config
	clientTLS *tls.Config
}

// New returns a plaintext WebSocket transport.
func New() *Transport { return &Transport{} }

// NewTLSServer returns a transport whose Listen serves wss:// using cert.
func NewTLSServer(cert tls.Certificate) *Transport {
	return &Transport{serverTLS: &tls.Config{Certificates: []tls.Certificate{cert}}}
}

// NewTLSClient returns a transport whose Dial connects over wss:// using
// cfg (typically mtls.PinnedClientConfig's result) to verify the server.
func NewTLSClient(cfg *tls.Config) *Transport {
	return &Transport{clientTLS: cfg}
}

// Dial opens a WebSocket connection to addr (host:port, no scheme).
func (t *Transport) Dial(ctx context.Context, addr string) (domain.Conn, error) {
	scheme := "ws"
	var opts *websocket.DialOptions
	if t.clientTLS != nil {
		scheme = "wss"
		opts = &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: t.clientTLS}}}
	}

	url := fmt.Sprintf("%s://%s/harness", scheme, addr)
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return nil, fmt.Errorf("ws: dial %s: %w", addr, err)
	}
	return &conn{ws: c, remote: addr}, nil
}

// Listen starts an HTTP server on addr and delivers each accepted
// WebSocket upgrade as a domain.Conn on the returned channel.
func (t *Transport) Listen(ctx context.Context, addr string) (<-chan domain.Conn, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ws: listen on %s: %w", addr, err)
	}
	if t.serverTLS != nil {
		ln = tls.NewListener(ln, t.serverTLS)
	}

	conns := make(chan domain.Conn)
	mux := http.NewServeMux()
	mux.HandleFunc("/harness", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		select {
		case conns <- &conn{ws: c, remote: r.RemoteAddr}:
		case <-ctx.Done():
			c.Close(websocket.StatusGoingAway, "server shutting down")
		}
	})

	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		close(conns)
	}()
	go server.Serve(ln)

	return conns, nil
}

// conn adapts a nhooyr.io/websocket connection to domain.Conn, framing each
// Send/Receive call as one WebSocket binary message (the protocol package
// owns the actual envelope encoding carried in that frame).
type conn struct {
	ws     *websocket.Conn
	remote string
}

func (c *conn) Send(ctx context.Context, data []byte) error {
	if err := c.ws.Write(ctx, websocket.MessageBinary, data); err != nil {
		return fmt.Errorf("ws: send: %w", err)
	}
	return nil
}

func (c *conn) Receive(ctx context.Context) ([]byte, error) {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("ws: receive: %w", err)
	}
	return data, nil
}

func (c *conn) RemoteAddr() string { return c.remote }

func (c *conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "closed")
}
