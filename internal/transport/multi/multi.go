// Package multi lets the manager accept connections from more than one
// domain.Transport at once — e.g. the direct LAN listener
// (internal/transport/ws) and a relay-backed listener
// (internal/transport/relay) simultaneously — without either the manager's
// core Server.Run loop or either transport needing to know the other
// exists. Composition happens entirely at the composition root
// (cmd/manager), matching how TLS configuration is already assembled there
// rather than inside Server.
package multi

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"home-harness/internal/domain"
)

// target pairs one transport with the address it should Listen on — addrs
// differ per transport (a LAN host:port vs. a relay server's address), so
// they can't be collapsed into the single addr string domain.Transport's
// Listen already takes.
type target struct {
	transport domain.Transport
	addr      string
}

// Transport fans in Listen across every added target into one channel. It
// only ever supports Listen — Dial has exactly one destination by nature,
// so a caller that wants to dial should use the specific underlying
// transport directly rather than through a Transport composed for
// Listening on several at once.
type Transport struct {
	targets []target
}

// New returns an empty composite transport; add targets with Add.
func New() *Transport { return &Transport{} }

// Add registers transport to also be listened on at addr when Listen is
// called, returning t so calls can be chained.
func (t *Transport) Add(transport domain.Transport, addr string) *Transport {
	t.targets = append(t.targets, target{transport, addr})
	return t
}

// Dial always fails: a composite of independently-addressed listeners has
// no single meaningful destination to dial. Callers that need to dial
// should hold and use the specific transport they mean to reach.
func (t *Transport) Dial(context.Context, string) (domain.Conn, error) {
	return nil, errors.New("multi: Dial is not supported; dial the underlying transport directly")
}

// Listen starts every added target listening (on its own configured addr,
// not the addr argument here, which is unused) and merges their accepted
// connections onto one channel, closed once every target's own channel has
// closed.
func (t *Transport) Listen(ctx context.Context, _ string) (<-chan domain.Conn, error) {
	if len(t.targets) == 0 {
		return nil, errors.New("multi: no transports added")
	}

	channels := make([]<-chan domain.Conn, 0, len(t.targets))
	for _, tg := range t.targets {
		ch, err := tg.transport.Listen(ctx, tg.addr)
		if err != nil {
			return nil, fmt.Errorf("multi: listen on %s: %w", tg.addr, err)
		}
		channels = append(channels, ch)
	}

	out := make(chan domain.Conn)
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(ch <-chan domain.Conn) {
			defer wg.Done()
			for {
				select {
				case c, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- c:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()

	return out, nil
}
