package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"home-harness/internal/domain"
)

// Spot checks (the manager's spotcheck.go): re-running a share of each
// job's checkable tasks on another device and comparing the results.

// spotCheckCounts is a job's spot checks by outcome.
type spotCheckCounts struct {
	Checking   int `json:"checking"`
	Matched    int `json:"matched"`
	Mismatch   int `json:"mismatch"`
	Unresolved int `json:"unresolved"`
	Skipped    int `json:"skipped"`
}

func (n *spotCheckCounts) String() string {
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{n.Matched, "matched"}, {n.Mismatch, "mismatch (a device was the odd one out)"}, {n.Unresolved, "differed, unresolved"}, {n.Checking, "checking"}, {n.Skipped, "skipped"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	return "spot checks: " + strings.Join(parts, ", ")
}

func describeSpotCheck(percent int) string {
	if percent <= 0 {
		return "Spot checks: off (harnessctl policy spot-check 10 re-runs 10% of each job's checkable tasks on another device)"
	}
	return fmt.Sprintf("Spot checks: %d%% of each job's checkable tasks (at least one) are re-run on another device and compared", percent)
}

// suspectNote flags a node in "harnessctl nodes" (neutral: spot checks
// found its results differing; nothing acts on it).
func suspectNote(m *domain.SuspectMark) string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("  [results differed from other devices' %d time(s): harnessctl node / clear-suspect]", m.Count)
}

// setSpotCheck is "policy spot-check <percent>|off".
func (c *apiClient) setSpotCheck(p domain.Policy, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: harnessctl policy spot-check <percent 1-100>|off")
	}
	percent := 0
	if args[0] != "off" {
		n, err := strconv.Atoi(strings.TrimSuffix(args[0], "%"))
		if err != nil || n < 0 || n > 100 {
			return fmt.Errorf("spot-check: %q is not a percentage from 0 to 100 (or off)", args[0])
		}
		percent = n
	}
	p.SpotCheckPercent = percent
	body, _ := json.Marshal(p)
	req, _ := http.NewRequest(http.MethodPut, c.base+"/policy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Println(describeSpotCheck(percent) + ".")
	if percent > 0 {
		fmt.Println("Applies to jobs submitted from now on. Only file.hash, text.count, image.resize and image.stack give the same bytes on every device, so only they are checked.")
	}
	return nil
}

// cmdClearSuspect is "clear-suspect <id>".
func (c *apiClient) cmdClearSuspect(id string) error {
	req, _ := http.NewRequest(http.MethodDelete, c.base+"/nodes/"+id+"/suspect", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Printf("Suspect mark cleared for %s.\n", id)
	return nil
}
