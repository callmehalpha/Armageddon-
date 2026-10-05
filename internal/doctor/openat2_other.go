//go:build !linux

package doctor

import "golang.org/x/sys/unix"

// probeOpenat2 reports ENOSYS: openat2 is Linux-only, and the server (and so
// the helper) runs on Linux. doctor still builds on macOS for the agent side.
func probeOpenat2(dir string) error { return unix.ENOSYS }
