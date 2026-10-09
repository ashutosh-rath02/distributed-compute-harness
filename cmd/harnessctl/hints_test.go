package main

import (
	"net"
	"strings"
	"testing"
)

// With nothing listening, the error says what to check, not just "connection refused".
func TestUnreachableManagerSaysWhatToDo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now

	c := newAPIClient("http://"+addr, "t")
	err = c.get("/nodes", &[]nodeView{})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"cannot reach the manager", "is it running", "-api-addr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}
