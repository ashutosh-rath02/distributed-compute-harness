package main

import (
	"net"
	"testing"
)

func TestLocalRelayAddr(t *testing.T) {
	for _, tc := range []struct{ bound, want string }{
		{"[::]:8420", "127.0.0.1:8420"},
		{"0.0.0.0:8420", "127.0.0.1:8420"},
		{"127.0.0.1:8420", "127.0.0.1:8420"},
		// A specific public bind isn't necessarily reachable on loopback,
		// so the gateway must dial it exactly as bound.
		{"203.0.113.5:8420", "203.0.113.5:8420"},
		{"[2001:db8::1]:8420", "[2001:db8::1]:8420"},
	} {
		addr, err := net.ResolveTCPAddr("tcp", tc.bound)
		if err != nil {
			t.Fatalf("ResolveTCPAddr(%q): %v", tc.bound, err)
		}
		if got := localRelayAddr(addr); got != tc.want {
			t.Errorf("localRelayAddr(%s) = %q, want %q", tc.bound, got, tc.want)
		}
	}
}
