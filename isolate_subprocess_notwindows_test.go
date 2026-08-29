//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// isolateSubprocess creates a new session for the child process on Unix,
// so it has no controlling terminal. This prevents bubbletea from opening
// /dev/tty and corrupting the parent's terminal state.
func isolateSubprocess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
