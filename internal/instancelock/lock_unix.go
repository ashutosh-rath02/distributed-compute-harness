//go:build !windows

package instancelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return ErrHeld
		}
		return err
	}
	return nil
}

func unlockFile(f *os.File) { unix.Flock(int(f.Fd()), unix.LOCK_UN) }
