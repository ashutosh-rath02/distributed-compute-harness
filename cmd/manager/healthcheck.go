package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// runHealthcheck asks the manager at apiAddr whether it is answering. It
// uses GET /server-proof, which needs no credential and reveals nothing, so
// a health check never has to hold the operator token. It returns the
// process exit code.
func runHealthcheck(apiAddr string) int {
	host, port, err := net.SplitHostPort(apiAddr)
	if err != nil {
		fmt.Println("unhealthy: bad -api-addr:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/server-proof?nonce=" + strings.Repeat("0", 32))
	if err != nil {
		fmt.Println("unhealthy:", err)
		return 1
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	// 200 is a manager with a token; 404 one without: both are the API answering.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		fmt.Println("ok")
		return 0
	}
	fmt.Println("unhealthy: the API answered", resp.Status)
	return 1
}
