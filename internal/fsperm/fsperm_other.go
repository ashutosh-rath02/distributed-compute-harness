//go:build !windows

package fsperm

func restrictToOwner(string) error { return nil }
