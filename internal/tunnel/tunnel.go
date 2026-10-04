// Package tunnel carries TCP connections between two devices through the
// manager, so a service that listens only on one device's loopback (such
// as llama.cpp's ggml-rpc-server, which has no authentication) can be used
// from another device without ever being reachable on the network.
//
// Each side opens a binary WebSocket to the manager's agent-facing TLS
// listener — the one it already pins — at /tunnel/{token}, with a token
// the manager issued for that side of that session alone; the manager
// splices the two byte for byte (manager/tunnel.go). Nothing new listens
// on the network, and no device trusts another directly.
package tunnel

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"

	"nhooyr.io/websocket"
)

// ReadLimit bounds one WebSocket message on a tunnel. Writers send what
// they read (io.Copy: 32 KiB at a time), so 1 MiB leaves ample room; the
// library's default (32 KiB) would cut a full chunk off.
const ReadLimit = 1 << 20

// Dial opens one side of a tunnel: a binary WebSocket to url (wss://
// manager/tunnel/<token>...) over client (pinned to the manager), as a
// net.Conn.
func Dial(ctx context.Context, url string, client *http.Client) (net.Conn, error) {
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(ReadLimit)
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
}

// Accept takes the manager's end of a tunnel side.
func Accept(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(ReadLimit)
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
}

// Splice copies between a and b in both directions until either side
// ends, then closes both and returns the bytes copied each way. A
// WebSocket has no half-close, so the end of one direction ends the
// whole connection — which is what a request/response protocol over it
// wants anyway.
func Splice(a, b net.Conn) (aToB, bToA int64) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		aToB, _ = io.Copy(b, a)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		bToA, _ = io.Copy(a, b)
		closeBoth()
	}()
	wg.Wait()
	return aToB, bToA
}
