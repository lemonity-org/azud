package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/server"
	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/ssh"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Setup servers and deploy",
	Long: `Setup servers and deploy the application.

This command performs a complete setup:
  1. Bootstraps servers (installs Podman)
  2. Logs into the container registry
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

	hosts := setupRuntimeHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	log.Info("Setting up %d server(s)...", len(hosts))

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	// Step 1: Bootstrap servers
	if !setupSkipBootstrap {
		log.Header("Step 1: Bootstrap Servers")
		bootstrapper := server.NewBootstrapper(sshClient, log, cfg.Podman.NetworkBackend)
		if err := bootstrapper.BootstrapAll(hosts); err != nil {
			return fmt.Errorf("bootstrap failed: %w", err)
		}

		if cfg.Podman.Rootless {
			var lingerErrors []string
			for _, host := range hosts {
				if err := enableLinger(sshClient, host, cfg.SSH.User); err != nil {
					log.HostError(host, "Failed to enable linger: %v", err)
					lingerErrors = append(lingerErrors, fmt.Sprintf("%s: %v", host, err))
				}
			}
			if len(lingerErrors) > 0 {
				return fmt.Errorf("failed to enable rootless Podman persistence: %s", strings.Join(lingerErrors, "; "))
			}
		}
	} else {
		log.Info("Skipping bootstrap (--skip-bootstrap)")
	}

	// Step 2: Sync secrets. Setup owns the complete first-deploy contract, so
	// users must not need a separate env push between bootstrap and deploy.
	log.Header("Step 2: Sync Secrets")
	if err := runEnvPush(cmd, args); err != nil {
		return fmt.Errorf("secret sync failed: %w", err)
	}

	// Step 3: Registry login
	log.Header("Step 3: Registry Login")
	if cfg.Registry.Username != "" {
		podmanClient := podman.NewClient(sshClient)
		registryManager := podman.NewRegistryManager(podmanClient)

		password := getRegistryPassword()
		if password == "" {
			return fmt.Errorf("registry password not found for configured registry user")
		} else {
			regConfig := &podman.RegistryConfig{
				Server:   cfg.Registry.Server,
				Username: cfg.Registry.Username,
				Password: password,
			}

			errors := registryManager.LoginAll(hosts, regConfig)
			if len(errors) > 0 {
				var loginErrors []string
				for host, err := range errors {
					log.HostError(host, "login failed: %v", err)
					loginErrors = append(loginErrors, fmt.Sprintf("%s: %v", host, err))
				}
				sort.Strings(loginErrors)
				return fmt.Errorf("registry login failed: %s", strings.Join(loginErrors, "; "))
			} else {
				log.Success("Registry login complete")
			}
		}
	} else {
		log.Info("No registry configured, skipping login")
	}

	// Step 4: Start proxy
	if !setupSkipProxy {
		log.Header("Step 4: Start Proxy")
		proxyManager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

		proxyConfig := &proxy.ProxyConfig{
			AutoHTTPS:             cfg.Proxy.SSL,
			Email:                 cfg.Proxy.ACMEEmail,
			Staging:               cfg.Proxy.ACMEStaging,
			SSLRedirect:           cfg.Proxy.SSLRedirect,
			HTTPPort:              cfg.Proxy.HTTPPort,
			HTTPSPort:             cfg.Proxy.HTTPSPort,
			LoggingEnabled:        cfg.Proxy.Logging.Enabled,
			RedactRequestHeaders:  cfg.Proxy.Logging.RedactRequestHeaders,
			RedactResponseHeaders: cfg.Proxy.Logging.RedactResponseHeaders,
		}
		if hosts := cfg.Proxy.AllHosts(); len(hosts) > 0 {
			proxyConfig.Hosts = hosts
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
				return fmt.Errorf("SSL certificate secrets not found: %s, %s", cfg.Proxy.SSLCertificate, cfg.Proxy.SSLPrivateKey)
			}
		}

		var proxyErrors []string
		proxyHosts := cfg.GetRoleHosts("web")
		if len(proxyHosts) == 0 {
			return fmt.Errorf("proxy setup requires a web role")
		}
		for _, host := range proxyHosts {
			if err := proxyManager.Boot(host, proxyConfig); err != nil {
				log.HostError(host, "proxy boot failed: %v", err)
				proxyErrors = append(proxyErrors, fmt.Sprintf("%s: %v", host, err))
			}
		}
		if len(proxyErrors) > 0 {
			return fmt.Errorf("proxy setup failed: %s", strings.Join(proxyErrors, "; "))
		}
	} else {
		log.Info("Skipping proxy setup (--skip-proxy)")
	}

	// Step 5: Deploy accessories
	if len(cfg.Accessories) > 0 {
		log.Header("Step 5: Deploy Accessories")
		if err := deployAccessories(sshClient, log); err != nil {
			return fmt.Errorf("accessory deployment failed: %w", err)
		}
	}

	// Step 6: Build and push
	if !setupSkipPush {
		log.Header("Step 6: Build and Push")
		if err := runBuild(cmd, args); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
	} else {
		log.Info("Skipping build (--skip-push)")
	}

	// Step 7: Deploy application
	log.Header("Step 7: Deploy Application")
	deployer := deploy.NewDeployer(cfg, sshClient, log)

	opts := &deploy.DeployOptions{
		SkipPull: false,
	}

	if err := deployer.Deploy(cmd.Context(), opts); err != nil {
		return fmt.Errorf("deploy failed: %w", err)
	}

	log.Header("Setup Complete!")
	log.Success("Your application is now running at:")
	proxyHosts := cfg.Proxy.AllHosts()
	if len(proxyHosts) == 0 {
		log.Warn("No proxy host configured")
		return nil
	}
	for _, host := range proxyHosts {
		if cfg.Proxy.SSL {
			log.Println("  https://%s", host)
		} else {
			log.Println("  http://%s", host)
		}
	}

	return nil
}

func setupRuntimeHosts() []string {
	seen := make(map[string]bool)
	var hosts []string
	for _, group := range [][]string{cfg.GetAllHosts(), cfg.GetAccessoryHosts(), cfg.GetAllCronHosts()} {
		for _, host := range group {
			if host != "" && !seen[host] {
				seen[host] = true
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
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

	// Try secrets file (loaded by config loader)
	if val, ok := config.GetSecret(secretKey); ok && val != "" {
		return val
	}

	return ""
}

func deployAccessories(sshClient *ssh.Client, log *output.Logger, selectedNames ...string) error {
	return deployAccessoriesOnHost(sshClient, log, "", selectedNames...)
}

func deployAccessoriesOnHost(sshClient *ssh.Client, log *output.Logger, selectedHost string, selectedNames ...string) error {
	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)
	imageManager := podman.NewImageManager(podmanClient)

	names := selectedNames
	if len(names) == 0 {
		names = cfg.GetAccessoryNames()
	}
	var errs []string
	for _, name := range names {
		accessory, ok := cfg.Accessories[name]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: not configured", name))
			continue
		}
		log.Info("Deploying accessory: %s", name)

		hosts := accessoryHosts(accessory)
		if selectedHost != "" {
			matched := false
			for _, host := range hosts {
				if host == selectedHost {
					hosts = []string{host}
					matched = true
					break
				}
			}
			if !matched {
				errs = append(errs, fmt.Sprintf("%s: host %s is not configured", name, selectedHost))
				continue
			}
		}
		if len(hosts) == 0 {
			log.Warn("No host configured for accessory %s, skipping", name)
			errs = append(errs, fmt.Sprintf("%s: no hosts configured", name))
			continue
		}
		for _, host := range hosts {

			if len(accessory.Env.Secret) > 0 {
				if err := ensureRemoteSecretsFile(sshClient, []string{host}, accessory.Env.Secret); err != nil {
					log.HostError(host, "Missing secrets for accessory %s: %v", name, err)
					errs = append(errs, fmt.Sprintf("%s@%s: missing secrets: %v", name, host, err))
					continue
				}
			}

			// Check if already running
			containerName := fmt.Sprintf("%s-%s", cfg.Service, name)
			running, err := containerManager.IsRunning(host, containerName)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s@%s inspect: %v", name, host, err))
				continue
			}
			if running {
				log.HostSuccess(host, "Accessory %s already running", name)
				continue
			}

			// `accessory boot` means start, not recreate. A stopped container still
			// owns its name, so attempting `podman run` would fail and, more
			// importantly, would discard the accessory's existing writable layer.
			exists, err := containerManager.Exists(host, containerName)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s@%s inspect: %v", name, host, err))
				continue
			}
			if exists {
				if err := containerManager.Start(host, containerName); err != nil {
					log.HostError(host, "Failed to start %s: %v", name, err)
					errs = append(errs, fmt.Sprintf("%s@%s: %v", name, host, err))
					continue
				}
				if bootTimeout := accessory.GetBootTimeout(); bootTimeout > 0 {
					if err := verifyAccessoryHealth(containerManager, host, containerName, name, bootTimeout, log); err != nil {
						log.HostError(host, "%v", err)
						errs = append(errs, fmt.Sprintf("%s@%s: %v", name, host, err))
						continue
					}
				}
				log.HostSuccess(host, "Accessory %s started", name)
				continue
			}

			if err := provisionAccessoryDirectories(sshClient, host, accessory.Directories); err != nil {
				log.HostError(host, "Failed to provision directories for %s: %v", name, err)
				errs = append(errs, fmt.Sprintf("%s@%s: %v", name, host, err))
				continue
			}
			// Upload files and add as volume mounts
			if err := uploadAccessoryFiles(sshClient, host, name, &accessory, log); err != nil {
				log.HostError(host, "Failed to provision files for %s: %v", name, err)
				errs = append(errs, fmt.Sprintf("%s@%s: %v", name, host, err))
				continue
			}

			// Build container config
			containerConfig := &podman.ContainerConfig{
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
			if len(containerConfig.SecretEnv) > 0 {
				containerConfig.EnvFile = config.RemoteSecretsPath(cfg)
			}

			// Add command if specified
			// Split command into arguments to preserve proper entrypoint behavior
			// (e.g., postgres needs to detect it's being run as 'postgres' to drop privileges)
			if accessory.Cmd != "" {
				containerConfig.Command = deploy.ParseCommandArgs(accessory.Cmd)
			}
			containerConfig.Memory = accessory.Options["memory"]
			containerConfig.CPUs = accessory.Options["cpus"]

			// Pull image
			if err := imageManager.Pull(host, accessory.Image); err != nil {
				log.HostError(host, "Failed to pull image for %s: %v", name, err)
				errs = append(errs, fmt.Sprintf("%s@%s: %v", name, host, err))
				continue
			}

			// Run container
			_, err = containerManager.Run(host, containerConfig)
			if err != nil {
				log.HostError(host, "Failed to start %s: %v", name, err)
				errs = append(errs, fmt.Sprintf("%s@%s: %v", name, host, err))
				continue
			}

			// Verify accessory is running and healthy
			bootTimeout := accessory.GetBootTimeout()
			if bootTimeout > 0 {
				if err := verifyAccessoryHealth(containerManager, host, containerName, name, bootTimeout, log); err != nil {
					log.HostError(host, "%v", err)
					errs = append(errs, fmt.Sprintf("%s@%s: %v", name, host, err))
					continue
				}
			}

			log.HostSuccess(host, "Accessory %s deployed", name)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%d accessory(ies) failed: %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

func accessoryHosts(accessory config.AccessoryConfig) []string {
	seen := make(map[string]struct{})
	var hosts []string
	for _, host := range append([]string{accessory.Host}, accessory.Hosts...) {
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts
}

func provisionAccessoryDirectories(sshClient *ssh.Client, host string, directories []string) error {
	for _, directory := range directories {
		result, err := sshClient.Execute(host, fmt.Sprintf("mkdir -p %s", shell.QuoteRemotePath(directory)))
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("mkdir %s failed: %s", directory, result.Stderr)
		}
	}
	return nil
}

// verifyAccessoryHealth checks that an accessory container is running and healthy.
// It first waits for a brief stabilization period to catch immediate crashes,
// then checks for a Podman HEALTHCHECK and waits for it if present.
func verifyAccessoryHealth(
	containerManager *podman.ContainerManager,
	host, containerName, accessoryName string,
	timeout time.Duration,
	log *output.Logger,
) error {
	// Phase 1: Stabilization check
	stabilize := 5 * time.Second
	if timeout < stabilize {
		stabilize = timeout
	}
	log.Host(host, "Waiting for %s to stabilize...", accessoryName)
	if err := containerManager.WaitRunning(host, containerName, stabilize); err != nil {
		return fmt.Errorf("accessory %s failed to start: %w", accessoryName, err)
	}

	// Phase 2: HEALTHCHECK polling (if the image defines one)
	hasHC, err := containerManager.HasHealthcheck(host, containerName)
	if err != nil {
		return fmt.Errorf("could not determine healthcheck status for %s: %w", accessoryName, err)
	}
	if !hasHC {
		return nil
	}

	remaining := timeout - stabilize
	if remaining <= 0 {
		remaining = 1 * time.Second
	}
	log.Host(host, "Waiting for %s healthcheck (timeout: %s)...", accessoryName, remaining)
	if err := containerManager.WaitHealthy(host, containerName, remaining); err != nil {
		return fmt.Errorf("accessory %s health check failed: %w", accessoryName, err)
	}

	return nil
}

func uploadAccessoryFiles(sshClient *ssh.Client, host, name string, accessory *config.AccessoryConfig, log *output.Logger) error {
	for _, f := range accessory.Files {
		dir := filepath.Dir(f.Remote)
		result, err := sshClient.Execute(host, fmt.Sprintf("mkdir -p %s", shell.QuoteRemotePath(dir)))
		if err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("creating directory %s: %s", dir, result.Stderr)
		}
		if err := sshClient.Upload(host, f.Local, f.Remote); err != nil {
			return fmt.Errorf("uploading %s to %s: %w", f.Local, f.Remote, err)
		}
		if f.Mode != "" {
			result, err := sshClient.Execute(host, fmt.Sprintf("chmod %s %s", shell.Quote(f.Mode), shell.QuoteRemotePath(f.Remote)))
			if err != nil {
				return fmt.Errorf("setting mode %s on %s: %w", f.Mode, f.Remote, err)
			}
			if result.ExitCode != 0 {
				return fmt.Errorf("setting mode %s on %s: %s", f.Mode, f.Remote, result.Stderr)
			}
		}
		if f.Owner != "" {
			result, err := sshClient.Execute(host, fmt.Sprintf("chown %s %s", shell.Quote(f.Owner), shell.QuoteRemotePath(f.Remote)))
			if err != nil {
				return fmt.Errorf("setting owner %s on %s: %w", f.Owner, f.Remote, err)
			}
			if result.ExitCode != 0 {
				return fmt.Errorf("setting owner %s on %s: %s", f.Owner, f.Remote, result.Stderr)
			}
		}
		accessory.Volumes = append(accessory.Volumes, fmt.Sprintf("%s:%s:ro", f.Remote, f.Remote))
		log.HostSuccess(host, "Uploaded %s for %s", f.Remote, name)
	}
	return nil
}
