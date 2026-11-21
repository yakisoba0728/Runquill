//go:build unix

package app

import (
	"os"
	"os/exec"
	"syscall"
)

// lockExclusive takes a non-blocking exclusive lock on the whole file.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// killProcessGroupOnCancel starts cmd in its own process group and kills the
// whole group on cancel so a helper process does not outlive cmd.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
