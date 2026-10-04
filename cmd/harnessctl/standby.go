package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// cmdStandby: a standby manager (roadmap item 17).
//
//	standby add             on the active manager: a one-time enrollment and the command for the standby
//	standby status          either manager: its role, term, and how current the copy is
//	standby promote [-force] on the standby: take over (refused while the primary answers, unless -force)
//	standby remove          on the active manager: forget the standby
func (c *apiClient) cmdStandby(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status":
		return c.standbyStatus()
	case "add":
		return c.standbyAdd()
	case "remove":
		if _, err := c.standbyCall(http.MethodDelete, "/standby", "", http.StatusOK); err != nil {
			return err
		}
		fmt.Println("Standby removed: it can no longer copy this manager's state, and won't take over.")
		return nil
	case "promote":
		fs := flag.NewFlagSet("standby promote", flag.ContinueOnError)
		force := fs.Bool("force", false, "promote even though the primary still answers (a planned switch-over: it steps down within about 10 seconds)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		body, err := c.standbyCall(http.MethodPost, "/standby/promote", fmt.Sprintf(`{"force":%v}`, *force), http.StatusOK)
		if err != nil {
			return err
		}
		var out struct {
			Term uint64 `json:"term"`
		}
		json.Unmarshal(body, &out)
		fmt.Printf("Promoted: this manager is now the active one (failover term %d). Devices that follow it reconnect within about 30 seconds.\n", out.Term)
		return nil
	default:
		return fmt.Errorf("usage: harnessctl standby [add | status | promote [-force] | remove]")
	}
}

func (c *apiClient) standbyCall(method, path, body string, want int) ([]byte, error) {
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		return nil, fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (c *apiClient) standbyAdd() error {
	body, err := c.standbyCall(http.MethodPost, "/standby", "", http.StatusCreated)
	if err != nil {
		return err
	}
	var enr struct {
		Token       string    `json:"token"`
		Fingerprint string    `json:"fingerprint"`
		Primary     string    `json:"primary"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(body, &enr); err != nil {
		return err
	}
	fmt.Printf("Standby enrollment created: single use, until %s.\n", enr.ExpiresAt.Local().Format("15:04"))
	fmt.Println("On the machine that will be the standby, start its manager once with:")
	fmt.Printf("  manager -standby-of %s -standby-fingerprint %s -standby-token %s -advertise-addr <its LAN address>:7420\n", enr.Primary, enr.Fingerprint, enr.Token)
	fmt.Println("(plus its usual -db / -tls-dir; add -force if it ran as a manager before: its database and identity are replaced by this one's.)")
	fmt.Println("It copies this manager's whole state, TLS key included, over the pinned connection, serves no devices until promoted,")
	fmt.Println("and stays a standby across restarts. Keep the token private. Adding a standby replaces any earlier one.")
	return nil
}

func (c *apiClient) standbyStatus() error {
	var raw map[string]any
	if err := c.get("/standby", &raw); err != nil {
		return err
	}
	data, _ := json.Marshal(raw)
	if raw["role"] == "standby" {
		var s struct {
			Primary                 string    `json:"primary"`
			Term                    uint64    `json:"term"`
			Copied                  bool      `json:"copied"`
			Unpaired                bool      `json:"unpaired"`
			LastCopyAt              time.Time `json:"lastCopyAt"`
			LastContactAt           time.Time `json:"lastContactAt"`
			LagSeconds              int64     `json:"lagSeconds"`
			PrimaryReachable        bool      `json:"primaryReachable"`
			AutoPromoteAfterSeconds int64     `json:"autoPromoteAfterSeconds"`
			Error                   string    `json:"error"`
		}
		json.Unmarshal(data, &s)
		fmt.Printf("This manager is a standby of %s (failover term %d): it serves no devices.\n", s.Primary, s.Term)
		switch {
		case !s.Copied:
			fmt.Println("Copy: none yet.")
		case s.LagSeconds >= 0:
			fmt.Printf("Copy: confirmed current %s ago (state last changed %s).\n", time.Duration(s.LagSeconds)*time.Second, ago(s.LastCopyAt))
		default:
			fmt.Printf("Copy: from %s; not confirmed since this manager started.\n", ago(s.LastCopyAt))
		}
		if s.PrimaryReachable {
			fmt.Println("The primary answers.")
		} else if !s.LastContactAt.IsZero() {
			fmt.Printf("The primary hasn't answered since %s.\n", ago(s.LastContactAt))
		} else {
			fmt.Println("The primary hasn't answered since this manager started.")
		}
		if s.Unpaired {
			fmt.Println("The primary no longer accepts this standby (another one was added, or it was removed).")
		}
		if s.AutoPromoteAfterSeconds > 0 {
			fmt.Printf("Takes over by itself after %s without the primary.\n", time.Duration(s.AutoPromoteAfterSeconds)*time.Second)
		} else {
			fmt.Println("Takes over only when promoted: harnessctl standby promote")
		}
		if s.Error != "" {
			fmt.Println("Last problem: " + s.Error)
		}
		return nil
	}
	var a struct {
		Role    string `json:"role"`
		Term    uint64 `json:"term"`
		Reason  string `json:"reason"`
		Standby *struct {
			Name       string    `json:"name"`
			Addr       string    `json:"addr"`
			AddedAt    time.Time `json:"addedAt"`
			LastSyncAt time.Time `json:"lastSyncAt"`
			LagSeconds int64     `json:"lagSeconds"`
			UpToDate   bool      `json:"upToDate"`
		} `json:"standby"`
		EnrollmentExpiresAt *time.Time `json:"enrollmentExpiresAt"`
	}
	json.Unmarshal(data, &a)
	if a.Role == "stepped-down" {
		fmt.Printf("This manager stepped down (failover term %d): a newer one took over.\n", a.Term)
	} else {
		fmt.Printf("This manager is the active one (failover term %d).\n", a.Term)
	}
	switch {
	case a.Reason != "":
		fmt.Println(a.Reason)
	case a.Standby == nil:
		fmt.Println("No standby. Add one with: harnessctl standby add")
	default:
		who := a.Standby.Addr
		if a.Standby.Name != "" {
			who = a.Standby.Name + " at " + a.Standby.Addr
		}
		state := "has not copied the state yet"
		if !a.Standby.LastSyncAt.IsZero() {
			state = fmt.Sprintf("last checked in %s", ago(a.Standby.LastSyncAt))
			if a.Standby.UpToDate {
				state += ", up to date"
			} else {
				state += ", behind"
			}
		}
		fmt.Printf("Standby: %s — %s.\n", who, state)
	}
	if a.EnrollmentExpiresAt != nil {
		fmt.Printf("A standby enrollment is open until %s.\n", a.EnrollmentExpiresAt.Local().Format("15:04"))
	}
	return nil
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}
