//go:build windows

package main

import (
	"os/exec"
)

// isolateSubprocess is a no-op on Windows.
func isolateSubprocess(cmd *exec.Cmd) {
}
