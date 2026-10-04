//go:build unix

package sysinfo

import (
	"errors"
	"syscall"
)

// LowerPriority runs this process (and the programs it starts, which
// inherit it) at nice 10, so the owner's own apps win the CPU over
// harness work. A process already nicer than that stays as it is (an
// unprivileged process can't raise its priority, and that refusal is
// not an error here).
func LowerPriority() error {
	err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
