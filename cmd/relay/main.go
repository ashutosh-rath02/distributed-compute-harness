// Command relay runs a small, disposable rendezvous relay: it never
// terminates TLS or parses WebSocket framing, only pairs and splices raw
// byte streams for a manager and agents that both know the same session
// token (internal/relay). It is the one component in this project meant
// to be reachable from the public internet — run it on anything with a
// stable public address (a cheap VPS is simplest; a home box works too if
// its router can port-forward one port to it).
//
// Known limitation: a node connected through this relay cannot self-update
// (see internal/relay's package comment for why) — SELF_UPDATE silently
// does nothing for it. Nodes on the manager's direct LAN listener are
// unaffected.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"home-harness/internal/relay"
)

func main() {
	addr := flag.String("addr", ":8420", "public address to listen on for manager and agent relay connections (host:port)")
	idleTimeout := flag.Duration("idle-timeout", relay.DefaultIdleTimeout, "how long a parked registration waits for a peer before it's given up on")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("relay: listen on %s: %v", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := relay.NewServer(*idleTimeout)
	log.Printf("relay listening on %s", *addr)
	if err := srv.Serve(ctx, ln); err != nil && ctx.Err() == nil {
		log.Fatalf("relay: %v", err)
	}
}
