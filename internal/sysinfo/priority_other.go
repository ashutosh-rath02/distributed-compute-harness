//go:build !windows && !unix

package sysinfo

// LowerPriority has nothing to do here.
func LowerPriority() error { return nil }
