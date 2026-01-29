package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/deploy"
	"github.com/adriancarayol/azud/internal/docker"
	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/proxy"
	"github.com/adriancarayol/azud/internal/server"
	"github.com/adriancarayol/azud/internal/ssh"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Setup servers and deploy",
	Long: `Setup servers and deploy the application.

This command performs a complete setup:
  1. Bootstraps servers (installs Docker)
  2. Logs into the Docker registry
  3. Starts the Caddy proxy
  4. Deploys accessories (databases, caches)
  5. Deploys the application

This is typically run once when setting up a new deployment.

Example:
  azud setup`,
	RunE: runSetup,
}

var (
	setupSkipBootstrap bool
	setupSkipProxy     bool
	setupSkipPush      bool
)

func init() {
	setupCmd.Flags().BoolVar(&setupSkipBootstrap, "skip-bootstrap", false, "Skip server bootstrap")
	setupCmd.Flags().BoolVar(&setupSkipProxy, "skip-proxy", false, "Skip proxy setup")
	setupCmd.Flags().BoolVar(&setupSkipPush, "skip-push", false, "Skip pushing the image")

	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	log.Header("Azud Setup")

	hosts := cfg.GetAllHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	log.Info("Setting up %d server(s)...", len(hosts))

	// Create SSH client
	sshClient := createSSHClient()
	defer sshClient.Close()

	// Step 1: Bootstrap servers
	if !setupSkipBootstrap {
		log.Header("Step 1: Bootstrap Servers")
		bootstrapper := server.NewBootstrapper(sshClient, log)
		if err := bootstrapper.BootstrapAll(hosts); err != nil {
			return fmt.Errorf("bootstrap failed: %w", err)
		}
	} else {
		log.Info("Skipping bootstrap (--skip-bootstrap)")
	}

	// Step 2: Registry login
	log.Header("Step 2: Registry Login")
	if cfg.Registry.Username != "" {
		dockerClient := docker.NewClient(sshClient)
		registryManager := docker.NewRegistryManager(dockerClient)

		password := getRegistryPassword()
		if password == "" {
			log.Warn("Registry password not found, skipping login")
		} else {
			regConfig := &docker.RegistryConfig{
				Server:   cfg.Registry.Server,
				Username: cfg.Registry.Username,
				Password: password,
			}

			errors := registryManager.LoginAll(hosts, regConfig)
			if len(errors) > 0 {
				for host, err := range errors {
					log.HostError(host, "login failed: %v", err)
				}
			} else {
				log.Success("Registry login complete")
			}
		}
	} else {
		log.Info("No registry configured, skipping login")
	}

	// Step 3: Start proxy
	if !setupSkipProxy {
		log.Header("Step 3: Start Proxy")
		proxyManager := proxy.NewManager(sshClient, log)

		proxyConfig := &proxy.ProxyConfig{
			AutoHTTPS: cfg.Proxy.SSL,
			Email:     cfg.Proxy.ACMEEmail,
			Staging:   cfg.Proxy.ACMEStaging,
			HTTPPort:  cfg.Proxy.HTTPPort,
			HTTPSPort: cfg.Proxy.HTTPSPort,
		}
		if len(cfg.Proxy.Hosts) > 0 {
			proxyConfig.Hosts = cfg.Proxy.Hosts
		} else if cfg.Proxy.Host != "" {
			proxyConfig.Hosts = []string{cfg.Proxy.Host}
		}

		// Load custom SSL certificates if configured
		if cfg.Proxy.SSLCertificate != "" && cfg.Proxy.SSLPrivateKey != "" {
			certPEM, certOK := config.GetSecret(cfg.Proxy.SSLCertificate)
			keyPEM, keyOK := config.GetSecret(cfg.Proxy.SSLPrivateKey)
			if certOK && keyOK {
				proxyConfig.SSLCertificate = certPEM
				proxyConfig.SSLPrivateKey = keyPEM
				log.Info("Using custom SSL certificates")
			} else {
				log.Warn("SSL certificate secrets not found: %s, %s", cfg.Proxy.SSLCertificate, cfg.Proxy.SSLPrivateKey)
			}
		}

		for _, host := range hosts {
			if err := proxyManager.Boot(host, proxyConfig); err != nil {
				log.HostError(host, "proxy boot failed: %v", err)
			}
		}
	} else {
		log.Info("Skipping proxy setup (--skip-proxy)")
	}

	// Step 4: Deploy accessories
	if len(cfg.Accessories) > 0 {
		log.Header("Step 4: Deploy Accessories")
		if err := deployAccessories(sshClient, log); err != nil {
			log.Warn("Accessory deployment had errors: %v", err)
		}
	}

	// Step 5: Build and push
	if !setupSkipPush {
		log.Header("Step 5: Build and Push")
		if err := runBuild(cmd, args); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
	} else {
		log.Info("Skipping build (--skip-push)")
	}

	// Step 6: Deploy application
	log.Header("Step 6: Deploy Application")
	deployer := deploy.NewDeployer(cfg, sshClient, log)

	opts := &deploy.DeployOptions{
		SkipPull: false,
	}

	if err := deployer.Deploy(opts); err != nil {
		return fmt.Errorf("deploy failed: %w", err)
	}

	log.Header("Setup Complete!")
	log.Success("Your application is now running at:")
	if cfg.Proxy.SSL {
		log.Println("  https://%s", cfg.Proxy.Host)
	} else {
		log.Println("  http://%s", cfg.Proxy.Host)
	}

	return nil
}

func getRegistryPassword() string {
	if len(cfg.Registry.Password) == 0 {
		return ""
	}

	secretKey := cfg.Registry.Password[0]

	// Try environment first
	if val := os.Getenv(secretKey); val != "" {
		return val
	}

	// Try secrets file
	// This would use the config's secrets loader
	return ""
}

func deployAccessories(sshClient *ssh.Client, log *output.Logger) error {
	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)

	for name, accessory := range cfg.Accessories {
		log.Info("Deploying accessory: %s", name)

		// Determine host
		host := accessory.Host
		if host == "" && len(accessory.Hosts) > 0 {
			host = accessory.Hosts[0]
		}
		if host == "" {
			log.Warn("No host configured for accessory %s, skipping", name)
			continue
		}

		// Check if already running
		containerName := fmt.Sprintf("%s-%s", cfg.Service, name)
		running, _ := containerManager.IsRunning(host, containerName)
		if running {
			log.HostSuccess(host, "Accessory %s already running", name)
			continue
		}

		// Build container config
		containerConfig := &docker.ContainerConfig{
			Name:    containerName,
			Image:   accessory.Image,
			Detach:  true,
			Restart: "unless-stopped",
			Network: "azud",
			Labels: map[string]string{
				"azud.managed":   "true",
				"azud.service":   cfg.Service,
				"azud.accessory": name,
			},
			Env:     make(map[string]string),
			Volumes: accessory.Volumes,
		}

		// Add port mapping
		if accessory.Port != "" {
			containerConfig.Ports = []string{accessory.Port}
		}

		// Add environment variables
		for key, value := range accessory.Env.Clear {
			containerConfig.Env[key] = value
		}
		containerConfig.SecretEnv = accessory.Env.Secret

		// Add command if specified
		// Split command into arguments to preserve proper entrypoint behavior
		// (e.g., postgres needs to detect it's being run as 'postgres' to drop privileges)
		if accessory.Cmd != "" {
			containerConfig.Command = strings.Fields(accessory.Cmd)
		}

		// Pull image
		imageManager := docker.NewImageManager(dockerClient)
		if err := imageManager.Pull(host, accessory.Image); err != nil {
			log.HostError(host, "Failed to pull image for %s: %v", name, err)
			continue
		}

		// Run container
		_, err := containerManager.Run(host, containerConfig)
		if err != nil {
			log.HostError(host, "Failed to start %s: %v", name, err)
			continue
		}

		log.HostSuccess(host, "Accessory %s deployed", name)
	}

	return nil
}
