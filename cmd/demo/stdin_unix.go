//go:build unix

package main

import "syscall"

// setFdNonblock toggles O_NONBLOCK on a file descriptor.
// Used by drainStdin to flush any pre-buffered keystrokes without
// blocking when there's nothing buffered.
func setFdNonblock(fd int, nonblock bool) error {
	return syscall.SetNonblock(fd, nonblock)
}
