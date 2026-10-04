package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/remoteaccess"
)

// Remote access (the manager's remote.go): the dashboard, and harnessctl
// itself, away from home through the relay.
//
//   - harnessctl remote [status|on [read-only|standard|full]|off|key|rotate|log]
//     manages it on the manager's own machine (these routes are never
//     available remotely);
//   - harnessctl -remote URL [-remote-key-file F] <command> runs any other
//     command from anywhere: requests are sealed with the remote key,
//     exactly like the remote page's, by code you hold rather than code
//     the relay serves (the page's one weak point; internal/remoteaccess).

const remoteKeyEnv = "HARNESS_REMOTE_KEY"

func cmdRemote(c *apiClient, args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "status":
		return c.remoteStatus(false)
	case "key":
		return c.remoteStatus(true)
	case "on":
		mode := manager.RemoteStandard
		if len(args) > 1 {
			mode = manager.RemoteMode(args[1])
		}
		if mode != manager.RemoteReadOnly && mode != manager.RemoteStandard && mode != manager.RemoteFull {
			return fmt.Errorf("usage: harnessctl remote on [read-only|standard|full]")
		}
		if err := c.remoteSend(http.MethodPut, "/remote-access", map[string]any{"enabled": true, "mode": mode}); err != nil {
			return err
		}
		return c.remoteStatus(true)
	case "off":
		if err := c.remoteSend(http.MethodPut, "/remote-access", map[string]any{"enabled": false}); err != nil {
			return err
		}
		fmt.Println("Remote access is off: every remote browser is signed out. The key is kept for when you turn it on again (\"harnessctl remote rotate\" replaces it).")
		return nil
	case "rotate":
		if err := c.remoteSend(http.MethodPost, "/remote-access/rotate", nil); err != nil {
			return err
		}
		fmt.Println("New remote key made: every remote browser is signed out, and the old key opens nothing.")
		return c.remoteStatus(true)
	case "log":
		fs := flag.NewFlagSet("remote log", flag.ContinueOnError)
		n := fs.Int("n", 30, "how many entries")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var entries []domain.AuditEntry
		if err := c.get(fmt.Sprintf("/audit?log=remote&limit=%d", *n), &entries); err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("No remote requests yet.")
			return nil
		}
		fmt.Printf("%-20s %-9s %-7s %-40s %s\n", "TIME", "SESSION", "METHOD", "PATH", "RESULT")
		for _, e := range entries {
			result := "allowed"
			if why, ok := e.Detail["refused"].(string); ok && why != "" {
				result = "refused: " + why
			}
			fmt.Printf("%-20s %-9v %-7v %-40v %s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Detail["session"], e.Detail["method"],
				truncate(fmt.Sprint(e.Detail["path"]), 40), truncate(result, 90))
		}
		return nil
	}
	return fmt.Errorf("usage: harnessctl remote [status | on [read-only|standard|full] | off | key | rotate | log [-n N]]")
}

func (c *apiClient) remoteSend(method, path string, body any) error {
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
		return fmt.Errorf("remote access: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

var remoteModeWords = map[manager.RemoteMode]string{
	manager.RemoteReadOnly: "view only",
	manager.RemoteStandard: "view and run tasks; revoking, policy, adding devices, labels and updates need full control",
	manager.RemoteFull:     "full control (still never the pairing token, the AI key, or these settings)",
}

func (c *apiClient) remoteStatus(showKey bool) error {
	var v manager.RemoteAccessView
	if err := c.get("/remote-access", &v); err != nil {
		return err
	}
	if !v.Enabled {
		fmt.Println("Remote access is off.")
		if !v.Available {
			fmt.Println(v.Unavailable)
		} else {
			fmt.Println("Turn it on with: harnessctl remote on [read-only|standard|full]")
		}
		return nil
	}
	fmt.Printf("Remote access is on: %s.\n", remoteModeWords[v.Mode])
	fmt.Printf("Address:  %s\n", v.URL)
	if showKey {
		fmt.Printf("Key:      %s\n", v.Key)
		fmt.Println("Open the address on your phone or laptop and enter the key; keep the key private.")
	} else {
		fmt.Println("Key:      (harnessctl remote key shows it)")
	}
	fmt.Printf("Signed in now: %d\n", v.Sessions)
	return nil
}

// newRemoteAPIClient is the client for harnessctl -remote: the operator
// API through the relay, sealed with the remote key.
func newRemoteAPIClient(url, keyFile string) (*apiClient, error) {
	key := strings.TrimSpace(os.Getenv(remoteKeyEnv))
	if key == "" && keyFile != "" {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, err
		}
		key = strings.TrimSpace(string(data))
	}
	if key == "" {
		return nil, fmt.Errorf("-remote needs the remote key: pass -remote-key-file or set %s", remoteKeyEnv)
	}
	if _, err := remoteaccess.NormalizeKey(key); err != nil {
		return nil, err
	}
	c := &apiClient{base: "http://manager"}
	c.http.Transport = &remoteaccess.Client{BaseURL: remoteaccess.BaseURLOf(url), Key: key}
	return c, nil
}
