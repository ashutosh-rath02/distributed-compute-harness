package domain

import "context"

// Conn is a single logical connection to one peer. Messages are opaque
// framed byte slices — encoding (the Harness Protocol envelope) is a
// concern of the protocol package, not of any given transport.
type Conn interface {
	Send(ctx context.Context, data []byte) error
	Receive(ctx context.Context) ([]byte, error)
	RemoteAddr() string
	Close() error
}

// Transport is the abstraction every concrete carrier (WebSocket today;
// QUIC, mesh, Bluetooth gateway, or relay later) must implement. Node and
// manager logic depend only on this interface, never on a concrete
// transport package (baseline §14).
type Transport interface {
	// Dial opens a Conn to a peer at addr (a transport-specific address).
	Dial(ctx context.Context, addr string) (Conn, error)
	// Listen starts accepting inbound connections at addr, delivering each
	// accepted Conn on the returned channel until ctx is canceled.
	Listen(ctx context.Context, addr string) (<-chan Conn, error)
}
