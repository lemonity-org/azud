package cli

import (
	"fmt"
	"strings"

	"github.com/adriancarayol/azud/internal/ssh"
)

func enableLinger(sshClient *ssh.Client, host, user string) error {
	if user == "" {
		user = "root"
	}
	cmd := fmt.Sprintf("loginctl enable-linger %s", user)
	result, err := sshClient.Execute(host, cmd)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 && !strings.Contains(result.Stderr, "No such") {
		return fmt.Errorf("enable-linger failed: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}
