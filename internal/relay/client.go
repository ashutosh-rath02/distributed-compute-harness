package relay

import (
	"context"
	"fmt"
	"net"
)

// hintedConn wraps a relay-spliced net.Conn so RemoteAddr reports the
// actual peer's address (as observed by the relay) rather than the
// relay's own address — every relay-arriving connection would otherwise
// report the same, unhelpful "remote addr" (the relay itself), since that
// is genuinely the only thing the underlying TCP connection knows.
type hintedConn struct {
	net.Conn
	peerAddr string
}

func (c *hintedConn) RemoteAddr() net.Addr {
	if c.peerAddr == "" {
		return c.Conn.RemoteAddr()
	}
	return relayPeerAddr(c.peerAddr)
}

// relayPeerAddr implements net.Addr for a display-only hint string — never
// dialed, never compared, purely informational (logs, RemoteAddr()).
type relayPeerAddr string

func (a relayPeerAddr) Network() string { return "relay" }
func (a relayPeerAddr) String() string  { return string(a) }

// DialListen registers as the "listen" side of session at the relay
// addr and blocks until a matching DialConnect pairs with it, ctx is
// canceled, or the relay's own idle timeout gives up on this registration
// first. Callers that want to keep accepting further pairings call
// DialListen again immediately after each one resolves — mirroring
// net.Listener.Accept being called in a loop.
func DialListen(ctx context.Context, addr, session string) (net.Conn, error) {
	return dial(ctx, addr, handshake{Role: roleListen, Session: session})
}

// DialConnect registers as the "connect" side of session at the relay
// addr. Unlike DialListen, this does not wait indefinitely for a peer —
// it pairs with a parked "listen", waiting at most the relay's short
// connect grace (well under a second) for one to re-register, and
// otherwise fails fast. Callers should retry on failure the same way they
// already retry any other transient dial failure.
func DialConnect(ctx context.Context, addr, session string) (net.Conn, error) {
	return dial(ctx, addr, handshake{Role: roleConnect, Session: session})
}

func dial(ctx context.Context, addr string, hs handshake) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("relay: dial %s: %w", addr, err)
	}

	// Unblocks the response read below as soon as ctx is done, instead of
	// waiting on a connection the caller no longer wants (relevant for
	// DialListen, which can otherwise wait up to the relay's full idle
	// timeout).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	if err := writeJSONLine(conn, hs); err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay: send handshake: %w", err)
	}

	var resp pairedResponse
	if err := readJSONLine(conn, &resp); err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("relay: %w", ctx.Err())
		}
		return nil, fmt.Errorf("relay: read response: %w", err)
	}
	if resp.Error != "" {
		conn.Close()
		if resp.Error == timedOutErrorText {
			return nil, ErrTimedOut
		}
		return nil, fmt.Errorf("relay: %s", resp.Error)
	}
	if !resp.Paired {
		conn.Close()
		return nil, fmt.Errorf("relay: unexpected response")
	}
	return &hintedConn{Conn: conn, peerAddr: resp.PeerAddr}, nil
}
