package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/protocol"
	"home-harness/internal/tunnel"
)

// Device-to-device tunnels (internal/tunnel, manager/tunnel.go), agent
// side. A helper exposes a service listening on its loopback for one
// session; when the main device connects through the manager, the manager
// sends TUNNEL_OPEN and the helper joins its service to the manager. The
// main device gets a loopback port per helper that leads there.

type exposedService struct {
	port  int
	token string
}

type tunnels struct {
	mu      sync.Mutex
	exposed map[string]exposedService // session/index -> local service
	// grants: tunnel tokens per assignment, for its run (split.go).
	grants map[domain.WorkloadID][]protocol.TunnelGrant
}

func tunnelKey(session string, index int) string { return session + "/" + strconv.Itoa(index) }

// ExposeService makes the service on 127.0.0.1:port reachable through
// session's tunnel number index, with the helper-side token the manager
// issued. The returned func withdraws it.
func (a *Agent) ExposeService(session string, index, port int, token string) func() {
	key := tunnelKey(session, index)
	a.tun.mu.Lock()
	if a.tun.exposed == nil {
		a.tun.exposed = map[string]exposedService{}
	}
	a.tun.exposed[key] = exposedService{port: port, token: token}
	a.tun.mu.Unlock()
	return func() {
		a.tun.mu.Lock()
		delete(a.tun.exposed, key)
		a.tun.mu.Unlock()
	}
}

// tunnelBase is the manager's agent-facing WebSocket base URL and the
// client pinned to it. Tunnels need a direct connection: through the
// relay they would carry gigabytes over someone else's server.
func (a *Agent) tunnelBase() (string, *http.Client, error) {
	if a.cfg.SelfUpdateBaseURL != "" {
		return "", nil, errors.New("tunnels need a direct (LAN) connection to the manager, not the relay")
	}
	addr := a.getCurrentManagerAddr()
	if addr == "" {
		return "", nil, errors.New("no known manager address")
	}
	scheme := "wss"
	if a.cfg.Insecure {
		scheme = "ws"
	}
	c := *a.selfUpdateHTTPClient()
	c.Timeout = 0
	return scheme + "://" + addr, &c, nil
}

func (a *Agent) handleTunnelOpen(ctx context.Context, env *protocol.Envelope) {
	var p protocol.TunnelOpenPayload
	if err := env.DecodePayload(&p); err != nil {
		return
	}
	a.tun.mu.Lock()
	svc, ok := a.tun.exposed[tunnelKey(p.Session, p.Index)]
	a.tun.mu.Unlock()
	if !ok {
		log.Printf("agent %s: tunnel for %s/%d: nothing exposed", a.identity.NodeID, p.Session, p.Index)
		return
	}
	go func() {
		local, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", svc.port), 5*time.Second)
		if err != nil {
			log.Printf("agent %s: tunnel %s/%d: local service: %v", a.identity.NodeID, p.Session, p.Index, err)
			return
		}
		base, client, err := a.tunnelBase()
		if err != nil {
			local.Close()
			log.Printf("agent %s: tunnel: %v", a.identity.NodeID, err)
			return
		}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		remote, err := tunnel.Dial(dctx, base+"/tunnel/"+svc.token+"?conn="+url.QueryEscape(p.ConnID), client)
		cancel()
		if err != nil {
			local.Close()
			log.Printf("agent %s: tunnel %s/%d: manager: %v", a.identity.NodeID, p.Session, p.Index, err)
			return
		}
		tunnel.Splice(local, remote)
	}()
}

// TunnelListen gives the main device a loopback port that leads, through
// the manager, to the service a helper exposed for the main-side token.
// It serves until ctx ends.
func (a *Agent) TunnelListen(ctx context.Context, token string) (string, error) {
	base, client, err := a.tunnelBase()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			local, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				remote, err := tunnel.Dial(dctx, base+"/tunnel/"+token, client)
				cancel()
				if err != nil {
					local.Close()
					log.Printf("agent %s: tunnel: %v", a.identity.NodeID, err)
					return
				}
				tunnel.Splice(local, remote)
			}()
		}
	}()
	return ln.Addr().String(), nil
}
