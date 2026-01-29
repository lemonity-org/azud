package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/docker"
	"github.com/adriancarayol/azud/internal/output"
)

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Manage Docker registry authentication",
	Long:  `Commands for managing Docker registry authentication on deployment servers.`,
}

var registryLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Login to Docker registry on all servers",
	Long: `Login to the configured Docker registry on all deployment servers.

The registry credentials are read from the configuration file and secrets.

Example:
  azud registry login              # Login on all servers
  azud registry login --host x.x.x # Login on specific host`,
	RunE: runRegistryLogin,
}

var registryLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logout from Docker registry on all servers",
	Long: `Logout from the configured Docker registry on all deployment servers.

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

	// Determine hosts
	var hosts []string
	if registryHost != "" {
		hosts = []string{registryHost}
	} else {
		hosts = cfg.GetAllHosts()
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts configured")
		}
	}

	log.Info("Logging into %s on %d host(s)...", server, len(hosts))

	// Create SSH client
	sshClient := createSSHClient()
	defer sshClient.Close()

	// Create registry manager
	dockerClient := docker.NewClient(sshClient)
	registryManager := docker.NewRegistryManager(dockerClient)

	// Login on all hosts
	registryConfig := &docker.RegistryConfig{
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
	var hosts []string
	if registryHost != "" {
		hosts = []string{registryHost}
	} else {
		hosts = cfg.GetAllHosts()
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts configured")
		}
	}

	log.Info("Logging out from %s on %d host(s)...", server, len(hosts))

	// Create SSH client
	sshClient := createSSHClient()
	defer sshClient.Close()

	// Create registry manager
	dockerClient := docker.NewClient(sshClient)
	registryManager := docker.NewRegistryManager(dockerClient)

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

// Additional registry utility commands

var registryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List images in a registry",
	Long: `List images in the configured registry.

This command uses the Docker registry API to list available images.

Example:
  azud registry list`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// This would require implementing registry API calls
		// For now, just show a helpful message
		server := cfg.Registry.Server
		if server == "" {
			server = "docker.io"
		}

		output.Info("Registry: %s", server)
		output.Info("Image: %s", cfg.Image)
		output.Println("")
		output.Println("To list tags, use:")
		output.Println("  docker images %s", cfg.Image)
		return nil
	},
}

// Helper function to mask password in logs
func maskPassword(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}
