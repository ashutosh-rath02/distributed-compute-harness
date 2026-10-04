package sysinfo

import "golang.org/x/sys/windows"

// LowerPriority runs this process (and the programs it starts, which
// inherit the class) below normal priority, so the owner's own apps win
// the CPU over harness work.
func LowerPriority() error {
	return windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)
}
