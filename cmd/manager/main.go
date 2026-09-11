// Command manager runs the harness control plane: it accepts agent
// connections, tracks the node registry, and (unless disabled) announces
// itself on the local network so agents don't need a hardcoded address.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"home-harness/internal/discovery/udp"
	"home-harness/internal/manager"
	"home-harness/internal/store/persistent"
	"home-harness/internal/transport/ws"
)

func main() {
	addr := flag.String("addr", ":7420", "address to listen on for agent connections (host:port)")
	apiAddr := flag.String("api-addr", ":7421", "address to serve the HTTP observability/control API on")
	dbPath := flag.String("db", "harness-manager.db", "path to the persistent store file")
	pairingToken := flag.String("pairing-token", "", "shared secret agents must present to register (required)")
	heartbeatTimeout := flag.Duration("heartbeat-timeout", 15*time.Second, "how long without a heartbeat before a node is marked offline")
	disableDiscovery := flag.Bool("disable-discovery", false, "disable the LAN multicast discovery beacon")
	flag.Parse()

	if *pairingToken == "" {
		log.Fatal("manager: -pairing-token is required")
	}

	store, err := persistent.Open(*dbPath)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}
	defer store.Close()

	srv := manager.NewServer(ws.New(), store, manager.Config{
		Addr:             *addr,
		PairingToken:     *pairingToken,
		HeartbeatTimeout: *heartbeatTimeout,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !*disableDiscovery {
		port, err := portOf(*addr)
		if err != nil {
			log.Fatalf("manager: -addr must include a port for discovery (or pass -disable-discovery): %v", err)
		}
		beacon := &udp.Beacon{ManagerPort: port}
		go func() {
			if err := beacon.Run(ctx); err != nil {
				log.Printf("manager: discovery beacon stopped: %v", err)
			}
		}()
	}

	apiServer := &http.Server{Addr: *apiAddr, Handler: srv.NewHTTPHandler()}
	go func() {
		if err := apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("manager: API server stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		apiServer.Shutdown(shutdownCtx)
	}()

	log.Printf("harness manager listening on %s (API on %s)", *addr, *apiAddr)
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("manager: %v", err)
	}
}

func portOf(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	return port, nil
}
