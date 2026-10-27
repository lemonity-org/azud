package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
)

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Manage container registry authentication",
	Long:  `Commands for managing container registry authentication on deployment servers.`,
}

var registryLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Login to container registry on all servers",
	Long: `Login to the configured container registry on all deployment servers.

The registry credentials are read from the configuration file and secrets.

Example:
  azud registry login              # Login on all servers
  azud registry login --host x.x.x # Login on specific host`,
	RunE: runRegistryLogin,
}

var registryLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logout from container registry on all servers",
	Long: `Logout from the configured container registry on all deployment servers.

Example:
  azud registry logout`,
	RunE: runRegistryLogout,
}

var (
	registryHost string
)

func init() {
	registryLoginCmd.Flags().StringVar(&registryHost, "host", "", "Specific host to login on")
	registryLogoutCmd.Flags().StringVar(&registryHost, "host", "", "Specific host to logout from")

	registryCmd.AddCommand(registryLoginCmd)
	registryCmd.AddCommand(registryLogoutCmd)
	rootCmd.AddCommand(registryCmd)
}

func runRegistryLogin(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	// Get registry configuration
	server := cfg.Registry.Server
	if server == "" {
		server = "docker.io"
	}

	username := cfg.Registry.Username
	if username == "" {
		return fmt.Errorf("registry username not configured")
	}

	// Determine hosts before resolving the password so an unconfigured host
	// is rejected without touching credentials.
	hosts, err := registryTargetHosts(cfg, registryHost)
	if err != nil {
		return err
	}

	// Get password from secrets
	password := ""
	if len(cfg.Registry.Password) > 0 {
		secretKey := cfg.Registry.Password[0]
		password = os.Getenv(secretKey)
		if password == "" {
			// Try loading from secrets file
			if p, ok := getSecret(secretKey); ok {
				password = p
			}
		}
	}

	if password == "" {
		return fmt.Errorf("registry password not found (secret: %v)", cfg.Registry.Password)
	}

	log.Info("Logging into %s on %d host(s)...", server, len(hosts))

	// Create SSH client
	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	// Create registry manager
	podmanClient := podman.NewClient(sshClient)
	registryManager := podman.NewRegistryManager(podmanClient)

	// Login on all hosts
	registryConfig := &podman.RegistryConfig{
		Server:   server,
		Username: username,
		Password: password,
	}

	errors := registryManager.LoginAll(hosts, registryConfig)

	// Report results
	successCount := len(hosts) - len(errors)
	for host, err := range errors {
		log.HostError(host, "login failed: %v", err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("login failed on %d/%d hosts", len(errors), len(hosts))
	}

	log.Success("Logged in on %d host(s)", successCount)
	return nil
}

func runRegistryLogout(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	server := cfg.Registry.Server
	if server == "" {
		server = "docker.io"
	}

	// Determine hosts
	hosts, err := registryTargetHosts(cfg, registryHost)
	if err != nil {
		return err
	}

	log.Info("Logging out from %s on %d host(s)...", server, len(hosts))

	// Create SSH client
	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	// Create registry manager
	podmanClient := podman.NewClient(sshClient)
	registryManager := podman.NewRegistryManager(podmanClient)

	// Logout on all hosts
	errors := registryManager.LogoutAll(hosts, server)

	// Report results
	successCount := len(hosts) - len(errors)
	for host, err := range errors {
		log.HostError(host, "logout failed: %v", err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("logout failed on %d/%d hosts", len(errors), len(hosts))
	}

	log.Success("Logged out on %d host(s)", successCount)
	return nil
}

// registryTargetHosts returns the hosts for registry login/logout. By default
// these are the server hosts. An explicit host must be a configured server,
// accessory, cron, or remote builder host; registry credentials are never sent
// to the SSH bastion or to a host that is not in the configuration.
func registryTargetHosts(cfg *config.Config, host string) ([]string, error) {
	if host == "" {
		hosts := cfg.GetAllHosts()
		if len(hosts) == 0 {
			return nil, fmt.Errorf("no hosts configured")
		}
		return hosts, nil
	}
	if containsString(cfg.GetWorkloadHosts(), host) || host == cfg.Builder.Remote.Host {
		return []string{host}, nil
	}
	if host == cfg.SSH.Proxy.Host {
		return nil, fmt.Errorf("host %s is the SSH bastion; registry credentials are only sent to server, accessory, cron, and builder hosts", host)
	}
	return nil, fmt.Errorf("host %s is not configured", host)
}
