package agent

import (
	"os/exec"
	"syscall"
)

// prepareChild has the kernel kill the program if the agent dies first
// (llama.cpp's servers must not outlive the agent; see the Windows file).
func prepareChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

func adoptChild(*exec.Cmd) {}
