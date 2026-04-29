//go:build windows

package controlplane

import "os/exec"

// setProcessGroup is a no-op on Windows. The exec.CommandContext
// cancellation behavior on Windows already terminates the process
// (CreateProcess + TerminateProcess). For deeper trees, Windows
// users may need a Job Object — out of scope for the initial v2.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing just the top process on
// Windows. Same caveat as setProcessGroup.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
