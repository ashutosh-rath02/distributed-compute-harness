package agent

import (
	"log"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A Windows child doesn't die with its parent: llama.cpp processes left
// behind by an agent that exited without canceling them (a self-update,
// a forced kill, a crash) would keep an unauthenticated ggml-rpc-server
// listening and gigabytes in use until reboot. So every one is put in a
// job object that kills its members when its last handle closes — the
// agent's, when the agent process ends, however it ends.

var childJob struct {
	once sync.Once
	h    windows.Handle
}

func killOnExitJob() windows.Handle {
	childJob.once.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			log.Printf("agent: job object for child programs: %v", err)
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			log.Printf("agent: job object for child programs: %v", err)
			windows.CloseHandle(h)
			return
		}
		childJob.h = h
	})
	return childJob.h
}

func prepareChild(*exec.Cmd) {}

// adoptChild puts a started program in the kill-on-exit job.
func adoptChild(cmd *exec.Cmd) {
	job := killOnExitJob()
	if job == 0 || cmd.Process == nil {
		return
	}
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		log.Printf("agent: tie %s to the agent: %v", cmd.Path, err)
		return
	}
	defer windows.CloseHandle(p)
	if err := windows.AssignProcessToJobObject(job, p); err != nil {
		log.Printf("agent: tie %s to the agent: %v", cmd.Path, err)
	}
}
