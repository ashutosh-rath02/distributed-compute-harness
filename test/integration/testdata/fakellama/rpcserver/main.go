// A stand-in for llama.cpp's ggml-rpc-server, for the split-session tests:
// same flags and start banner; each connection answers every line it gets
// with "helper-<port>: <line>". It writes a small file into its cache dir
// ($LLAMA_CACHE) the way -c does.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

var version = "9999"

func main() {
	host := flag.String("H", "127.0.0.1", "host")
	port := flag.Int("p", 50052, "port")
	cache := flag.Bool("c", false, "cache")
	showVersion := flag.Bool("version", false, "version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("version: %s (fakehash)\n", version)
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir := os.Getenv("LLAMA_CACHE")
	if *cache && dir != "" {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "piece.bin"), make([]byte, 2<<20), 0o600)
	}
	fmt.Printf("Starting RPC server v3.0.0\n  endpoint       : %s\n  local cache    : %s\n", ln.Addr(), dir)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				fmt.Fprintf(c, "helper-%d: %s\n", *port, sc.Text())
			}
		}()
	}
}
