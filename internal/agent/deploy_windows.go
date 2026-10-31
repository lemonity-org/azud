//go:build windows

package agent

import "os/exec"

// configureDeployProcess keeps exec's default: a timeout kills the command.
// The agent does not run on Windows; this only keeps the package building.
func configureDeployProcess(cmd *exec.Cmd) {}

// killDeployProcessGroup is a no-op on Windows.
func killDeployProcessGroup(cmd *exec.Cmd) {}
