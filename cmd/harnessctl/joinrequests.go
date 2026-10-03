package main

import (
	"encoding/json"
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

// cmdJoinWindow shows, opens or closes the join window: while it is open,
// new devices may ask to join and the join page serves its installers.
func (c *apiClient) cmdJoinWindow(args []string) error {
	method, body := http.MethodGet, ""
	if len(args) > 0 {
		switch args[0] {
		case "open":
			method, body = http.MethodPost, "{}"
			if len(args) > 1 {
				body = fmt.Sprintf(`{"minutes":%s}`, strings.TrimSuffix(args[1], "m"))
			}
		case "close":
			method = http.MethodDelete
		default:
			return fmt.Errorf("usage: harnessctl join-window [open [minutes] | close]")
		}
	}
	req, err := http.NewRequest(method, c.base+"/join-window", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("join window: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var v struct {
		Open             bool   `json:"open"`
		RemainingSeconds int    `json:"remainingSeconds"`
		URL              string `json:"url"`
		Port             string `json:"port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	if !v.Open {
		fmt.Println("Adding devices is closed. Open it with: harnessctl join-window open [minutes]")
		return nil
	}
	where := v.URL
	if where == "" && v.Port != "" {
		where = "http://<this machine's address>:" + v.Port
	}
	fmt.Printf("Adding devices is open for %s more.", (time.Duration(v.RemainingSeconds) * time.Second).String())
	if where != "" {
		fmt.Printf(" On the new device, open %s", where)
	}
	fmt.Println()
	return nil
}
