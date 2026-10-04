//go:build !windows && !linux

package agent

import "os/exec"

// No portable way to tie a child's life to the agent here (macOS): the
// sweep at start (llamaCpp.sweep) clears any left behind.
func prepareChild(*exec.Cmd) {}

func adoptChild(*exec.Cmd) {}
