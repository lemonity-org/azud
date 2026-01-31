package cli

import (
	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/deploy"
	"github.com/adriancarayol/azud/internal/ssh"
)

func ensureRemoteSecretsFile(sshClient *ssh.Client, hosts []string, requiredKeys []string) error {
	return deploy.ValidateRemoteSecrets(sshClient, hosts, config.RemoteSecretsPath(cfg), requiredKeys)
}
