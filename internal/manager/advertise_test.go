package manager

import (
	"testing"
	"time"
)

func TestAdvertiseAddrFollowsTheNetwork(t *testing.T) {
	cur := "192.168.0.10:7420"
	calls := 0
	s := &Server{cfg: Config{AdvertiseFunc: func() string { calls++; return cur }}}

	if got := s.advertiseAddr(); got != "192.168.0.10:7420" {
		t.Fatalf("first look: %q", got)
	}
	cur = "10.20.195.183:7420"
	if got := s.advertiseAddr(); got != "192.168.0.10:7420" || calls != 1 {
		t.Fatalf("within the cache window it must not look again: %q after %d looks", got, calls)
	}
	s.advertAt = time.Now().Add(-time.Minute)
	if got := s.advertiseAddr(); got != "10.20.195.183:7420" {
		t.Fatalf("after the window it must see the new network: %q", got)
	}
}

func TestAdvertiseAddrFixedByOperatorWins(t *testing.T) {
	s := &Server{cfg: Config{AdvertiseAddr: "1.2.3.4:7420", AdvertiseFunc: func() string { return "9.9.9.9:7420" }}}
	if got := s.advertiseAddr(); got != "1.2.3.4:7420" {
		t.Fatalf("got %q", got)
	}
	if got := (&Server{}).advertiseAddr(); got != "" {
		t.Fatalf("no address known: %q", got)
	}
}
