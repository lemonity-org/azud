//go:build windows

package local

import "os/exec"

// configureProcess is a no-op: newLocalConnection refuses Windows.
func configureProcess(command *exec.Cmd) {}
