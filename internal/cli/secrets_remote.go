package cli

import (
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/ssh"
)

func ensureRemoteSecretsFile(sshClient *ssh.Client, hosts []string, requiredKeys []string) error {
	return deploy.ValidateRemoteSecrets(sshClient, hosts, config.RemoteSecretsPath(cfg), requiredKeys)
}
