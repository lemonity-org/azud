//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// configureDeployProcess starts the deploy command in its own process group
// and makes a timeout send SIGTERM to that whole group.
func configureDeployProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

// killDeployProcessGroup kills every process left in the deploy command's
// group. A group that is already gone is not an error.
func killDeployProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
