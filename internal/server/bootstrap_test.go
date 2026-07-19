package server

import (
	"strings"
	"testing"
)

func TestPodmanInstallCommandsUseNonInteractiveSudo(t *testing.T) {
	bootstrapper := &Bootstrapper{}
	commands := []string{
		bootstrapper.getDebianPodmanInstall("sudo -n "),
		bootstrapper.getRHELPodmanInstall("sudo -n "),
		bootstrapper.getAlpinePodmanInstall("sudo -n "),
	}
	for _, command := range commands {
		for _, line := range strings.Split(command, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line == "set -e" {
				continue
			}
			if !strings.HasPrefix(line, "sudo -n ") {
				t.Fatalf("privileged install line is not non-interactive: %q", line)
			}
		}
	}
}
