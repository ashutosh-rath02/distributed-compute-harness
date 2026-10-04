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

// cmdSplit shows, starts or stops the split session: one model run
// across several devices with llama.cpp (the manager's split.go).
func (c *apiClient) cmdSplit(args []string) error {
	if len(args) == 0 {
		return c.showSplit()
	}
	switch args[0] {
	case "start":
		fs := flag.NewFlagSet("split start", flag.ContinueOnError)
		main := fs.String("main", "", "the device that has the model file (node id)")
		model := fs.String("model", "", "the model file (.gguf) on that device")
		name := fs.String("name", "", "the name chats use for it, e.g. big-model")
		helpers := fs.String("helpers", "", "comma-separated helper node ids (default: every other device with llama.cpp that is taking work)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *main == "" || *model == "" || *name == "" {
			return fmt.Errorf("usage: harnessctl split start -main ID -model PATH.gguf -name NAME [-helpers ID,ID]")
		}
		req := map[string]any{"main": *main, "model": *model, "name": *name}
		if *helpers != "" {
			req["helpers"] = strings.Split(*helpers, ",")
		}
		body, _ := json.Marshal(req)
		resp, err := c.http.Post(c.base+"/llm/split", "application/json", strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		}
		fmt.Println("Starting. Loading a big model onto every device can take a few minutes:")
		for i := 0; i < 3600; i++ {
			v, err := c.split()
			if err != nil {
				return err
			}
			if v == nil || v.State == "ready" || v.State == "stopped" {
				return c.showSplit()
			}
			time.Sleep(time.Second)
		}
		return c.showSplit()
	case "stop":
		req, _ := http.NewRequest(http.MethodDelete, c.base+"/llm/split", nil)
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("manager returned %s", resp.Status)
		}
		fmt.Println("Stopping.")
		return nil
	}
	return fmt.Errorf("usage: harnessctl split [start -main ID -model PATH.gguf -name NAME [-helpers ID,ID] | stop]")
}

type splitSession struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Model    string    `json:"model"`
	Main     string    `json:"main"`
	Helpers  []string  `json:"helpers"`
	State    string    `json:"state"`
	Reason   string    `json:"reason"`
	Started  time.Time `json:"started"`
	LastUsed time.Time `json:"lastUsed"`
}

func (c *apiClient) split() (*splitSession, error) {
	var v struct {
		Session *splitSession `json:"session"`
	}
	return v.Session, c.get("/llm/split", &v)
}

func (c *apiClient) showSplit() error {
	s, err := c.split()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Println("No split session. Start one with: harnessctl split start -main ID -model PATH.gguf -name NAME")
		return nil
	}
	fmt.Printf("Session   %s: %s\n", s.ID, s.State)
	if s.Reason != "" {
		fmt.Printf("Why       %s\n", s.Reason)
	}
	fmt.Printf("Model     %s (chat with it as %q)\n", s.Model, s.Name)
	fmt.Printf("Main      %s\n", s.Main)
	fmt.Printf("Helpers   %s\n", strings.Join(s.Helpers, ", "))
	if s.State == "ready" {
		fmt.Printf("Last chat %s ago (it stops after 30 minutes without one)\n", time.Since(s.LastUsed).Round(time.Second))
	}
	return nil
}
