package cli

import (
	"fmt"
	"strings"

	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/ssh"
)

func enableLinger(sshClient *ssh.Client, host, user string) error {
	cmd := lingerCommand(user)
	result, err := sshClient.Execute(host, cmd)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 && !strings.Contains(result.Stderr, "No such") {
		return fmt.Errorf("enable-linger failed: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func lingerCommand(user string) string {
	if user == "" {
		user = "root"
	}
	prefix := ""
	if user != "root" {
		// Enabling persistence for a login user is a privileged host change.
		// Non-interactive SSH sessions cannot satisfy a PolicyKit prompt, so
		// require the same explicit passwordless sudo contract as bootstrap.
		prefix = "sudo -n "
	}
	return fmt.Sprintf("%sloginctl enable-linger %s", prefix, shell.Quote(user))
}
