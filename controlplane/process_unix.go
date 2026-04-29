//go:build !windows

package controlplane

import (
	"os/exec"
	"syscall"
)

// setProcessGroup configures the command to run in its own process
// group on Unix. This is what makes killProcessGroup able to send
// SIGKILL to the entire tree of descendants when the context
// times out.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to the entire process group started
// by cmd. Negative PID is the syscall convention for "the whole
// process group with this PGID".
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
