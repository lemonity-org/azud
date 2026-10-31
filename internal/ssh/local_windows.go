//go:build windows

package ssh

import "os/exec"

// configureLocalProcess is a no-op: newLocalConnection refuses Windows.
func configureLocalProcess(command *exec.Cmd) {}
