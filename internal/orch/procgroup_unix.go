//go:build unix

package orch

import (
	"os/exec"
	"syscall"
)

// ownGroup runs cmd in a process group of its own, so everything it starts (test servers,
// watchers, builds) can be killed with it. Cancelling cmd's context kills the whole group.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
}

// killGroup kills cmd's process group: whatever it left running after it exited, or everything
// when it's stopped.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
