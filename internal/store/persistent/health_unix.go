//go:build !windows

package persistent

import "syscall"

func noSpaceErrno(e syscall.Errno) bool {
	return e == syscall.ENOSPC || e == syscall.EROFS || e == syscall.EDQUOT
}

func readOnlyErrno(e syscall.Errno) bool { return e == syscall.EROFS }
