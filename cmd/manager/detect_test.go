package main

import (
	"net"
	"testing"
)

// The address offered to devices is a guess: only a private address, and
// only when the manager listens on every interface.
func TestDetectLANAddr(t *testing.T) {
	for _, bound := range []string{"127.0.0.1:7420", "192.168.1.5:7420", "nonsense"} {
		if got := detectLANAddr(bound); got != "" {
			t.Errorf("listening on %s: offered %q, want nothing", bound, got)
		}
	}
	got := detectLANAddr(":7420")
	if got == "" {
		t.Skip("no private network address on this machine")
	}
	host, port, err := net.SplitHostPort(got)
	if err != nil || port != "7420" || !net.ParseIP(host).IsPrivate() {
		t.Fatalf("offered %q: want a private address with port 7420", got)
	}
}
