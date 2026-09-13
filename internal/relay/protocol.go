// Package relay implements a minimal rendezvous relay: two peers that each
// know the same session token connect to the relay from anywhere on the
// internet and are spliced into one bidirectional raw byte stream. Beyond
// one handshake line per connection, the relay never inspects the bytes it
// forwards — no TLS termination, no WebSocket framing — so it is a pure,
// untrusted forwarder. The manager's own TLS fingerprint pinning and
// per-node REGISTER signature (internal/mtls, internal/identity) are what
// actually authenticate the two ends; they run end-to-end through whatever
// this package splices together, unaffected by the relay's own behavior.
//
// Authorization at the relay is a single unguessable bearer token per
// session, sent in plaintext as part of the handshake line below. That is
// a deliberate choice, not an oversight: the token only ever decides which
// two raw TCP connections get spliced together, never grants access to
// anything else, since everything carried over the resulting pipe is
// independently secured one layer up (or, in -insecure dev mode, exactly
// as unauthenticated as LAN plaintext mode already is). Treat a session
// token with the same care as the existing manager pairing token: long,
// random, and not reused across deployments.
//
// Known limitation: self-update (internal/agent/selfupdate.go) does not
// work for a relay-connected node. It downloads the new binary with a
// plain HTTP GET to the manager's address, and in relay mode that address
// is the relay's — which only speaks this package's rendezvous protocol,
// not HTTP. The download fails and is logged (no crash, no corruption:
// performSelfUpdate leaves the running binary untouched on any failure
// before the swap), but SELF_UPDATE then permanently does nothing for that
// node — there is no way for the manager to distinguish a relay-arrived
// connection from a LAN one (RemoteAddr reports the agent's real address
// either way, via hintedConn) to reject the attempt up front instead.
// Fixing this means routing the download itself through the relay, which
// is separate follow-up work, not part of v5 remote part 1.
package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxHandshakeLine bounds the one-line handshake/response messages below —
// generous for a JSON object holding a token and an address, small enough
// that a misbehaving peer can't make the relay buffer unbounded data
// before role/session are even known.
const maxHandshakeLine = 4096

// role identifies which side of a pairing a connection is registering as.
type role string

const (
	roleListen  role = "listen"
	roleConnect role = "connect"
)

// handshake is the first and only line a connection sends before the relay
// either parks it (listen) or attempts to pair it (connect).
type handshake struct {
	Role    role   `json:"role"`
	Session string `json:"session"`
}

// pairedResponse is the one line the relay sends back once a pairing
// succeeds or fails. PeerAddr is the other side's observed remote address
// (as seen by the relay, not the peer itself) — purely a display hint for
// logs on the receiving side, since the relay itself is the only party
// that can see both ends.
type pairedResponse struct {
	Paired   bool   `json:"paired"`
	PeerAddr string `json:"peerAddr,omitempty"`
	Error    string `json:"error,omitempty"`
}

// timedOutErrorText is the exact Error string the relay sends a "listen"
// registration that hits DefaultIdleTimeout with no peer ever showing up.
// It is a shared constant (not duplicated as a literal in both server.go
// and client.go) specifically so client.go can recognize it and map it
// back to ErrTimedOut — a listener's scheduled re-registration, not a
// failure, and the two must be told apart on the wire since a home-scale
// pool of parked registrations hits this constantly under completely
// normal operation.
const timedOutErrorText = "timed out waiting for a peer"

// ErrTimedOut is returned by DialListen when the relay gives up on a
// parked registration before any DialConnect claimed it. Callers that keep
// a pool of registrations open (internal/transport/relay's listener)
// should treat this as an expected, silent cue to re-register immediately
// — not a failure worth logging or backing off from — since it fires
// continuously on a perfectly healthy manager with no genuine problem.
var ErrTimedOut = errors.New("relay: " + timedOutErrorText)

// readLine reads a single '\n'-terminated line from r, one byte at a time.
//
// This is deliberately not bufio.Reader: bufio reads ahead in chunks, so
// any bytes of the tunneled stream that arrive immediately after the
// handshake line could end up sitting in bufio's internal buffer instead
// of on the wire — invisible to a later io.Copy(conn, ...) splice, which
// reads from the raw net.Conn directly. A byte-at-a-time read costs
// nothing measurable for a message this small and guarantees zero bytes
// are consumed beyond the line itself.
func readLine(r io.Reader) ([]byte, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, b[0])
			if len(buf) > maxHandshakeLine {
				return nil, fmt.Errorf("relay: handshake line exceeds %d bytes", maxHandshakeLine)
			}
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return nil, fmt.Errorf("relay: connection closed mid-line")
			}
			return nil, err
		}
	}
}

func readJSONLine(r io.Reader, v any) error {
	line, err := readLine(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}
