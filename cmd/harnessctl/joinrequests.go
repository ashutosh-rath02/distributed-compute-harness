package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"home-harness/internal/manager"
)

// Devices waiting to join by approval (agent -pair, the join page's
// installers): list them, approve one after comparing its code, or reject.

func (c *apiClient) cmdJoinRequests() error {
	var reqs []manager.JoinRequest
	if err := c.get("/join-requests", &reqs); err != nil {
		return err
	}
	if len(reqs) == 0 {
		fmt.Println("No devices are waiting for approval.")
		return nil
	}
	fmt.Printf("%-24s %-9s %-20s %-16s %-16s %s\n", "NODE ID", "CODE", "NAME", "PLATFORM", "FROM", "WAITING")
	for _, r := range reqs {
		state := time.Since(r.RequestedAt).Round(time.Second).String()
		if r.Approved {
			state = "approved, connecting"
		}
		fmt.Printf("%-24s %-9s %-20s %-16s %-16s %s\n", r.NodeID, r.Code, truncate(r.Name, 20),
			r.Platform.OS+"/"+r.Platform.Architecture, r.Remote, state)
	}
	return nil
}

// cmdDecideJoin approves or rejects a waiting device, named by node ID or
// by its pairing code (with or without the space).
func (c *apiClient) cmdDecideJoin(which string, approve bool) error {
	var reqs []manager.JoinRequest
	if err := c.get("/join-requests", &reqs); err != nil {
		return err
	}
	want := strings.ReplaceAll(which, " ", "")
	var match []manager.JoinRequest
	for _, r := range reqs {
		if string(r.NodeID) == which || strings.ReplaceAll(r.Code, " ", "") == want {
			match = append(match, r)
		}
	}
	switch len(match) {
	case 0:
		return fmt.Errorf("no device waiting for approval matches %q (see: harnessctl join-requests)", which)
	case 1:
	default:
		return fmt.Errorf("%q matches %d waiting devices; use the node ID", which, len(match))
	}
	r := match[0]
	verb := "reject"
	if approve {
		verb = "approve"
	}
	resp, err := c.http.Post(c.base+"/join-requests/"+string(r.NodeID)+"/"+verb, "application/json", nil)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if approve {
		fmt.Printf("Approved %s (%s, code %s). It connects within a few seconds.\n", r.Name, r.NodeID, r.Code)
	} else {
		fmt.Printf("Rejected %s (%s). It is refused for the next 10 minutes.\n", r.Name, r.NodeID)
	}
	return nil
}
