// Package udp implements local-network discovery for v0: the manager
// periodically announces itself over IPv4 multicast, and an agent listens
// for that announcement to learn a dialable address without any hardcoded
// configuration (v1.md §4.1). It is scoped to the local network by IPv4
// multicast's default TTL of 1.
package udp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"home-harness/internal/protocol"
)

// DefaultMulticastAddr is the multicast group/port both the beacon and the
// discoverer use unless overridden.
const DefaultMulticastAddr = "239.255.42.99:38899"

const defaultBeaconInterval = 2 * time.Second
const defaultDiscoveryTimeout = 10 * time.Second

// beaconPayload is what the manager announces. It carries only the port:
// the discoverer learns the manager's address from the source IP of the
// received packet, which sidesteps having to pick a LAN IP to advertise
// on a multi-homed machine.
type beaconPayload struct {
	ProtocolVersion string `json:"protocolVersion"`
	ManagerPort     int    `json:"managerPort"`
}

// Beacon periodically announces the manager's WS listen port over
// multicast.
type Beacon struct {
	// MulticastAddr defaults to DefaultMulticastAddr if empty.
	MulticastAddr string
	// ManagerPort is the TCP port the manager's transport listens on.
	ManagerPort int
	// Interval defaults to 2s if zero.
	Interval time.Duration
}

// Run broadcasts beacons until ctx is canceled.
func (b *Beacon) Run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp4", b.multicastAddr())
	if err != nil {
		return fmt.Errorf("udp: resolve multicast addr: %w", err)
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return fmt.Errorf("udp: dial multicast group: %w", err)
	}
	defer conn.Close()

	data, err := json.Marshal(beaconPayload{
		ProtocolVersion: protocol.Version,
		ManagerPort:     b.ManagerPort,
	})
	if err != nil {
		return fmt.Errorf("udp: marshal beacon: %w", err)
	}

	interval := b.Interval
	if interval == 0 {
		interval = defaultBeaconInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Send one immediately so discovery doesn't wait a full interval on a
	// freshly-started manager.
	conn.Write(data)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			conn.Write(data)
		}
	}
}

func (b *Beacon) multicastAddr() string {
	if b.MulticastAddr != "" {
		return b.MulticastAddr
	}
	return DefaultMulticastAddr
}

// Discoverer implements domain.Discoverer by listening for a manager's
// Beacon on the local network.
type Discoverer struct {
	// MulticastAddr defaults to DefaultMulticastAddr if empty.
	MulticastAddr string
	// Timeout defaults to 10s if zero.
	Timeout time.Duration
}

// Discover blocks until a compatible manager beacon is received, the
// timeout elapses, or ctx is canceled.
func (d *Discoverer) Discover(ctx context.Context) (string, error) {
	addr, err := net.ResolveUDPAddr("udp4", d.multicastAddr())
	if err != nil {
		return "", fmt.Errorf("udp: resolve multicast addr: %w", err)
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		return "", fmt.Errorf("udp: join multicast group: %w", err)
	}
	defer conn.Close()

	timeout := d.Timeout
	if timeout == 0 {
		timeout = defaultDiscoveryTimeout
	}
	conn.SetReadDeadline(time.Now().Add(timeout))

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 1024)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("udp: no manager beacon received within %s: %w", timeout, err)
		}

		var payload beaconPayload
		if err := json.Unmarshal(buf[:n], &payload); err != nil {
			continue // ignore malformed packets from unrelated senders
		}
		if protocol.Major(payload.ProtocolVersion) != protocol.Major(protocol.Version) {
			continue // ignore beacons from an incompatible manager version
		}
		return fmt.Sprintf("%s:%d", src.IP.String(), payload.ManagerPort), nil
	}
}

func (d *Discoverer) multicastAddr() string {
	if d.MulticastAddr != "" {
		return d.MulticastAddr
	}
	return DefaultMulticastAddr
}
