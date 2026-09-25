//go:build !unix

package orch

import "os/exec"

// ownGroup and killGroup only kill the process itself where process groups don't exist.
func ownGroup(cmd *exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
