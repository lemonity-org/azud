package cli

import (
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/local"
)

func ensureRemoteSecretsFile(sshClient local.Runner, hosts []string, requiredKeys []string) error {
	return deploy.ValidateRemoteSecrets(sshClient, hosts, config.RemoteSecretsPath(cfg), requiredKeys)
}
