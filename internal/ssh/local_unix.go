//go:build !windows

package ssh

import (
	"os/exec"
	"syscall"
)

// configureLocalProcess starts the shell in its own process group so a
// timeout or cancellation reaches everything it started (podman, flock),
// not just the shell.
func configureLocalProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
}
