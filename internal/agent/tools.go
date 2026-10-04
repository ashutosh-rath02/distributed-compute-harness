package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// DefaultToolsDir is where the agent looks first for the programs the
// media and document task types run (ffmpeg, whisper.cpp and its models,
// poppler, tesseract) when -tools-dir isn't given: a "tools" folder next
// to the agent's own data (%LOCALAPPDATA%\HomeHarness\tools on Windows,
// ~/.home-harness/tools elsewhere). Never filled by the agent.
func DefaultToolsDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "HomeHarness", "tools")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".home-harness", "tools")
}

// runProgram runs one of those programs tied to the agent's life
// (childproc_*.go), so a transcode doesn't outlive a killed agent.
func runProgram(cmd *exec.Cmd) error {
	exited, err := startProgram(cmd)
	if err != nil {
		return err
	}
	return <-exited
}
