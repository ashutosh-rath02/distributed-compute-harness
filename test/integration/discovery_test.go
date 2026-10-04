package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/discovery/udp"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/transport/ws"
)

func TestAgentDiscoversManagerOverMulticast(t *testing.T) {
	// Bind broadly (not 127.0.0.1) so the address the discoverer derives
	// from the beacon packet's source IP is actually the one the WS
	// listener is reachable on.
	const wsAddr = "0.0.0.0:19199"
	const wsPort = 19199
	const multicastGroup = "239.255.42.7:38944" // distinct from other tests' groups

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := manager.NewServer(ws.New(), nil, manager.Config{
		Addr:             wsAddr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
	})
	go srv.Run(ctx)
	waitListening(t, wsAddr)

	beacon := &udp.Beacon{MulticastAddr: multicastGroup, ManagerPort: wsPort, Interval: 100 * time.Millisecond}
	go beacon.Run(ctx)

	// Probe multicast delivery directly before trusting the full agent
	// reconnect loop to surface it. Some networks/hosts silently drop IPv4
	// multicast below the firewall/OS level (confirmed on this project's
	// dev machine, where no firewall rule could fix it) — the manual-
	// address fallback (v1.md §4.1) is what makes v0 not depend on this
	// working everywhere, so skip rather than fail when it doesn't.
	probe := &udp.Discoverer{MulticastAddr: multicastGroup, Timeout: 3 * time.Second}
	if _, err := probe.Discover(ctx); err != nil {
		t.Skipf("no beacon received, likely multicast unsupported in this environment: %v", err)
	}

	// Deliberately no ManagerAddr: the agent must find the manager purely
	// via discovery, per v1.md §4.1 ("without manually configuring IP
	// addresses").
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn,
		Discoverer:        &udp.Discoverer{MulticastAddr: multicastGroup, Timeout: 3 * time.Second},
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "discovered-agent"),
		Name:              "discovered-agent",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)

	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
}

func TestManagerAddrOverrideSkipsDiscovery(t *testing.T) {
	const addr = "127.0.0.1:19200"
	srv := startManager(t, addr, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A Discoverer that always fails: if the agent ever consulted it, the
	// connection would never succeed. Setting ManagerAddr must bypass it
	// entirely (the "manual fallback" from v1.md §4.1).
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn,
		ManagerAddr:       addr,
		Discoverer:        alwaysFailDiscoverer{},
		PairingToken:      pairingToken,
		IdentityDir:       filepath.Join(t.TempDir(), "manual-agent"),
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	go a.Run(ctx)

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
}

type alwaysFailDiscoverer struct{}

func (alwaysFailDiscoverer) Discover(context.Context) (string, error) {
	panic("Discoverer must not be consulted when ManagerAddr is set")
}
