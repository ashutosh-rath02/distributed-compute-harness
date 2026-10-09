package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Users and key rotation (manager/users.go, secrets.go). All of it needs
// an admin credential: the operator token, or an admin user's token.
//
//	harnessctl user add NAME viewer|operator|admin [days]  make a user; its token is shown once
//	harnessctl user list
//	harnessctl user remove NAME
//	harnessctl user token NAME          a new token (shown once); the old one stops at once
//	harnessctl user rotate NAME [mins]  a new token; the old one works for mins more (default 10)
//	harnessctl rotate operator-token|ai-key|pairing-token

func (c *apiClient) callJSON(method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func cmdUser(c *apiClient, args []string) error {
	usage := fmt.Errorf("usage: harnessctl user add NAME viewer|operator|admin [days] | list | remove NAME | token NAME | rotate NAME [minutes]")
	if len(args) == 0 {
		return usage
	}
	var tok struct {
		Name  string `json:"name"`
		Role  string `json:"role"`
		Token string `json:"token"`
		Grace int    `json:"graceMinutes"`
	}
	switch args[0] {
	case "add":
		if len(args) < 3 || len(args) > 4 {
			return usage
		}
		days := 0
		if len(args) == 4 {
			n, err := strconv.Atoi(args[3])
			if err != nil || n < 1 {
				return fmt.Errorf("days must be a whole number of days, 1 or more")
			}
			days = n
		}
		if err := c.callJSON(http.MethodPost, "/users", map[string]any{"name": args[1], "role": args[2], "expiresDays": days}, &tok); err != nil {
			return err
		}
		fmt.Printf("User %s (%s) added.\n", tok.Name, tok.Role)
		showUserToken(tok.Token)
	case "list":
		var users []struct {
			Name     string     `json:"name"`
			Role     string     `json:"role"`
			Created  time.Time  `json:"created"`
			LastUsed *time.Time `json:"lastUsed"`
			Expires  *time.Time `json:"expires"`
			Expired  bool       `json:"expired"`
		}
		if err := c.callJSON(http.MethodGet, "/users", nil, &users); err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Println("No users yet: the operator token is the only credential. Add one with \"harnessctl user add NAME ROLE\".")
		}
		for _, u := range users {
			last, exp := "never used", "no expiry"
			if u.LastUsed != nil {
				last = "last used " + u.LastUsed.Local().Format("2006-01-02 15:04")
			}
			if u.Expires != nil {
				exp = "expires " + u.Expires.Local().Format("2006-01-02")
				if u.Expired {
					exp = "EXPIRED " + u.Expires.Local().Format("2006-01-02")
				}
			}
			fmt.Printf("%-20s %-9s %s, %s\n", u.Name, u.Role, last, exp)
		}
	case "remove":
		if len(args) != 2 {
			return usage
		}
		if err := c.callJSON(http.MethodDelete, "/users/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("User %s removed: its token and every browser it signed in are refused from now on.\n", args[1])
	case "token":
		if len(args) != 2 {
			return usage
		}
		if err := c.callJSON(http.MethodPost, "/users/"+url.PathEscape(args[1])+"/token", nil, &tok); err != nil {
			return err
		}
		fmt.Printf("New token for %s. The old one has stopped working and its browsers are signed out.\n", args[1])
		showUserToken(tok.Token)
	case "rotate":
		if len(args) < 2 || len(args) > 3 {
			return usage
		}
		body := map[string]any{}
		if len(args) == 3 {
			n, err := strconv.Atoi(args[2])
			if err != nil || n < 0 {
				return fmt.Errorf("minutes must be a whole number, 0 or more")
			}
			body["graceMinutes"] = n
		}
		if err := c.callJSON(http.MethodPost, "/users/"+url.PathEscape(args[1])+"/rotate", body, &tok); err != nil {
			return err
		}
		fmt.Printf("New token for %s. The old one keeps working for %d more minutes; browsers are signed out now.\n", args[1], tok.Grace)
		showUserToken(tok.Token)
	default:
		return usage
	}
	return nil
}

func showUserToken(token string) {
	fmt.Println()
	fmt.Println("  " + token)
	fmt.Println()
	fmt.Println("This is shown once and the manager keeps only a hash of it: copy it now. Use it as")
	fmt.Println("HARNESS_OPERATOR_TOKEN for harnessctl, or sign in to the dashboard with it.")
}

func cmdRotate(c *apiClient, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: harnessctl rotate operator-token|ai-key|pairing-token")
	}
	var out struct {
		Rotated string `json:"rotated"`
		Value   string `json:"value"`
		File    string `json:"file"`
	}
	if err := c.callJSON(http.MethodPost, "/rotate/"+url.PathEscape(args[0]), nil, &out); err != nil {
		return err
	}
	switch out.Rotated {
	case "operator-token":
		fmt.Println("The operator token is replaced, with no restart. The old token and every dashboard sign-in stop working now (users' own tokens are unaffected).")
		if out.File != "" {
			fmt.Println("The manager saved it to", out.File+": harnessctl run from that folder, and \"harnessctl login-url\", already use it. Copy it to any other machine that runs harnessctl:")
		}
		fmt.Println("\n  " + out.Value + "\n")
	case "ai-key":
		fmt.Println("The AI key is replaced, with no restart. Apps using the old key get 401 until you give them the new one (dashboard \"Use with AI apps\", or harnessctl ai). A standby picks it up at its next copy.")
	case "pairing-token":
		fmt.Println("The pairing token is replaced, with no restart. Devices already added keep working (they reconnect with their own key). A device that still holds the old token can no longer join; invitations and approvals are unaffected. Launchers that read the pairing-token file next to the database pick the new one up; ones with the old value typed into them need editing.")
	}
	return nil
}
