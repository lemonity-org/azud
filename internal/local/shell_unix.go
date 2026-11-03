//go:build !windows

package local

import (
	"os/exec"
	"syscall"
)

// configureProcess starts the shell in its own process group so a
// timeout or cancellation reaches everything it started (podman, flock),
// not just the shell.
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
}
