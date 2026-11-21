//go:build windows

package app

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// The manager and worker run on Linux. These fallbacks only keep the remote
// CLI and workflow check buildable on Windows developer machines.

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
)

var procLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

// lockExclusive takes a non-blocking exclusive lock on the whole file.
func lockExclusive(f *os.File) error {
	var overlapped syscall.Overlapped
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&overlapped)))
	if r == 0 {
		return err
	}
	return nil
}

// killProcessGroupOnCancel keeps the default cancel, which kills cmd only:
// Windows has no POSIX process groups.
func killProcessGroupOnCancel(*exec.Cmd) {}
