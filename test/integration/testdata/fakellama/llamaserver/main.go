// A stand-in for llama.cpp's llama-server with --rpc, for the split-session
// tests: it connects to every --rpc endpoint and keeps the connections (as
// llama.cpp does), serves /health and an OpenAI chat endpoint whose answer
// asks every helper again — proving the tunnels carry each request — and
// exits when a helper connection breaks (llama.cpp can't lose a helper).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

var version = "9999"

type helper struct {
	mu sync.Mutex
	c  net.Conn
	r  *bufio.Reader
}

func (h *helper) ask(line string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := fmt.Fprintln(h.c, line); err != nil {
		return "", err
	}
	reply, err := h.r.ReadString('\n')
	return strings.TrimSpace(reply), err
}

func main() {
	model := flag.String("m", "", "model")
	rpc := flag.String("rpc", "", "rpc servers")
	flag.String("ngl", "", "gpu layers")
	host := flag.String("host", "127.0.0.1", "host")
	port := flag.Int("port", 8080, "port")
	alias := flag.String("alias", "", "alias")
	showVersion := flag.Bool("version", false, "version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("version: %s (fakehash)\n", version)
		return
	}
	if _, err := os.Stat(*model); err != nil {
		fmt.Fprintln(os.Stderr, "no model file:", err)
		os.Exit(1)
	}
	var helpers []*helper
	for _, addr := range strings.Split(*rpc, ",") {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rpc:", err)
			os.Exit(1)
		}
		h := &helper{c: c, r: bufio.NewReader(c)}
		if _, err := h.ask("hello"); err != nil {
			fmt.Fprintln(os.Stderr, "rpc hello:", err)
			os.Exit(1)
		}
		helpers = append(helpers, h)
	}
	fmt.Printf("loaded %s over %d rpc servers\n", *model, len(helpers))
	broken := func(err error) {
		fmt.Fprintln(os.Stderr, "lost a helper:", err)
		os.Exit(2)
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string }
			Stream   bool
		}
		json.NewDecoder(r.Body).Decode(&req)
		var answers []string
		for _, h := range helpers {
			a, err := h.ask("chat")
			if err != nil {
				http.Error(w, "lost a helper", 500)
				go broken(err)
				return
			}
			answers = append(answers, a)
		}
		last := ""
		if n := len(req.Messages); n > 0 {
			last = req.Messages[n-1].Content
		}
		reply := fmt.Sprintf("[%s] split over %d helpers (%s): %s", *alias, len(helpers), strings.Join(answers, "; "), last)
		if !req.Stream {
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": reply}, "finish_reason": "stop"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, piece := range strings.SplitAfter(reply, " ") {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": piece}, "finish_reason": nil}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":11}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	if err := http.ListenAndServe(fmt.Sprintf("%s:%d", *host, *port), nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
