package persistent

import "syscall"

// ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL, ERROR_WRITE_PROTECT, and the
// ERROR_DISK_QUOTA_EXCEEDED a quota raises.
func noSpaceErrno(e syscall.Errno) bool {
	return e == 112 || e == 39 || e == 19 || e == 1295 || e == syscall.ENOSPC || e == syscall.EROFS
}

func readOnlyErrno(e syscall.Errno) bool { return e == 19 || e == syscall.EROFS }
