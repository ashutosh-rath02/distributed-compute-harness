// Command relay runs a small, disposable rendezvous relay: it never
// terminates TLS or parses WebSocket framing, only pairs and splices raw
// byte streams for a manager and agents that both know the same session
// token (internal/relay). It is the one component in this project meant
// to be reachable from the public internet — run it on anything with a
// stable public address (a cheap VPS is simplest; a home box works too if
// its router can port-forward one port to it).
//
// Auxiliary HTTP connections such as agent self-update downloads use the
// same rendezvous path and retain end-to-end TLS to the manager.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"home-harness/internal/relay"
)

func main() {
	addr := flag.String("addr", ":8420", "public address to listen on for manager and agent relay connections (host:port)")
	idleTimeout := flag.Duration("idle-timeout", relay.DefaultIdleTimeout, "how long a parked registration waits for a peer before it's given up on")
	publicAddr := flag.String("public-addr", "", "separate HTTPS address for public enrollment links (disabled if empty)")
	publicTLSCert := flag.String("public-tls-cert", "", "PEM certificate for -public-addr (required when enabled; use a browser-trusted certificate)")
	publicTLSKey := flag.String("public-tls-key", "", "PEM private key for -public-addr")
	aliasKey := flag.String("alias-key", "", "stable secret used to issue opaque per-device relay credentials (required with -public-addr; preserve across restarts)")
	publishToken := flag.String("enrollment-publish-token", "", "secret managers must present to publish internet enrollment links (required with -public-addr)")
	flag.Parse()
	if *publicAddr != "" && (*publicTLSCert == "" || *publicTLSKey == "" || *aliasKey == "" || *publishToken == "") {
		log.Fatal("relay: -public-tls-cert, -public-tls-key, -alias-key, and -enrollment-publish-token are required with -public-addr")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("relay: listen on %s: %v", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := relay.NewServerWithAliasKey(*idleTimeout, *aliasKey)
	if *publicAddr != "" {
		publicServer := &http.Server{
			Addr: *publicAddr, Handler: relay.NewPublicGateway(srv, localRelayAddr(ln.Addr()), *publishToken).Handler(),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
		}
		go func() {
			if err := publicServer.ListenAndServeTLS(*publicTLSCert, *publicTLSKey); err != nil && err != http.ErrServerClosed {
				log.Printf("relay public enrollment server stopped: %v", err)
				stop()
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			publicServer.Shutdown(shutdownCtx)
		}()
		log.Printf("relay public enrollment HTTPS listening on %s", *publicAddr)
	}
	log.Printf("relay listening on %s", *addr)
	if err := srv.Serve(ctx, ln); err != nil && ctx.Err() == nil {
		log.Fatalf("relay: %v", err)
	}
}

// localRelayAddr is the address the public gateway dials to reach this
// same process's rendezvous listener. A wildcard bind (":8420",
// "0.0.0.0:8420", "[::]:8420") is reachable on loopback; a specific bind
// ("203.0.113.5:8420") may not be, so it is dialed exactly as bound.
func localRelayAddr(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
		return addr.String()
	}
	return net.JoinHostPort("127.0.0.1", port)
}
