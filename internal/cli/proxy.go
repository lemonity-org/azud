package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/proxy"
)

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Manage the Caddy reverse proxy",
	Long:  `Commands for managing the Caddy reverse proxy on deployment servers.`,
}

var proxyBootCmd = &cobra.Command{
	Use:   "boot",
	Short: "Start the proxy on servers",
	Long: `Start the Caddy reverse proxy on all deployment servers.

The proxy handles:
  - Routing requests to application containers
  - Automatic HTTPS via Let's Encrypt
  - Zero-downtime deployments
  - Health checking

Example:
  azud proxy boot              # Start on all servers
  azud proxy boot --host x.x.x # Start on specific host`,
	RunE: runProxyBoot,
}

var proxyStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the proxy on servers",
	Long: `Stop the Caddy reverse proxy on deployment servers.

Example:
  azud proxy stop`,
	RunE: runProxyStop,
}

var proxyRebootCmd = &cobra.Command{
	Use:   "reboot",
	Short: "Restart the proxy on servers",
	Long: `Restart the Caddy reverse proxy on deployment servers.

Example:
  azud proxy reboot`,
	RunE: runProxyReboot,
}

var proxyLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View proxy logs",
	Long: `View logs from the Caddy reverse proxy.

Example:
  azud proxy logs              # View recent logs
  azud proxy logs -f           # Follow logs
  azud proxy logs --tail 100   # Last 100 lines`,
	RunE: runProxyLogs,
}

var proxyStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show proxy status",
	Long: `Show the status of the Caddy reverse proxy on all servers.

Example:
  azud proxy status`,
	RunE: runProxyStatus,
}

var proxyRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove the proxy container",
	Long: `Remove the Caddy proxy container from servers.

This will stop and remove the container. Use with caution.

Example:
  azud proxy remove`,
	RunE: runProxyRemove,
}

var (
	proxyHost        string
	proxyFollow      bool
	proxyTail        string
	proxyForceRemove bool
)

func init() {
	// Boot flags
	proxyBootCmd.Flags().StringVar(&proxyHost, "host", "", "Specific host to operate on")

	// Stop flags
	proxyStopCmd.Flags().StringVar(&proxyHost, "host", "", "Specific host to operate on")

	// Reboot flags
	proxyRebootCmd.Flags().StringVar(&proxyHost, "host", "", "Specific host to operate on")

	// Logs flags
	proxyLogsCmd.Flags().StringVar(&proxyHost, "host", "", "Specific host to get logs from")
	proxyLogsCmd.Flags().BoolVarP(&proxyFollow, "follow", "f", false, "Follow log output")
	proxyLogsCmd.Flags().StringVar(&proxyTail, "tail", "100", "Number of lines to show")

	// Status flags
	proxyStatusCmd.Flags().StringVar(&proxyHost, "host", "", "Specific host to check")

	// Remove flags
	proxyRemoveCmd.Flags().StringVar(&proxyHost, "host", "", "Specific host to operate on")
	proxyRemoveCmd.Flags().BoolVar(&proxyForceRemove, "force", false, "Force removal")

	// Add subcommands
	proxyCmd.AddCommand(proxyBootCmd)
	proxyCmd.AddCommand(proxyStopCmd)
	proxyCmd.AddCommand(proxyRebootCmd)
	proxyCmd.AddCommand(proxyLogsCmd)
	proxyCmd.AddCommand(proxyStatusCmd)
	proxyCmd.AddCommand(proxyRemoveCmd)

	rootCmd.AddCommand(proxyCmd)
}

func runProxyBoot(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getTargetHosts(proxyHost)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	// Run pre-proxy-reboot hook
	hooks := newHookRunner()
	hookCtx := newHookContext()
	hookCtx.Hosts = strings.Join(hosts, ",")
	if err := hooks.Run(cmd.Context(), "pre-proxy-reboot", hookCtx); err != nil {
		return fmt.Errorf("pre-proxy-reboot hook failed: %w", err)
	}

	// Create proxy manager
	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	manager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

	// Build proxy config
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
			log.Warn("SSL certificate secrets not found: %s, %s", cfg.Proxy.SSLCertificate, cfg.Proxy.SSLPrivateKey)
		}
	}

	// Boot on all hosts
	var bootErrors []string
	var succeededHosts []string
	for _, host := range hosts {
		if err := manager.Boot(host, proxyConfig); err != nil {
			log.HostError(host, "failed to boot proxy: %v", err)
			bootErrors = append(bootErrors, fmt.Sprintf("%s: %v", host, err))
			continue
		}
		succeededHosts = append(succeededHosts, host)
	}

	if len(bootErrors) == len(hosts) {
		return fmt.Errorf("proxy boot failed on all hosts: %s", strings.Join(bootErrors, "; "))
	}

	// Run post-proxy-reboot hook (only with succeeded hosts)
	hookCtx.Hosts = strings.Join(succeededHosts, ",")
	if err := hooks.Run(cmd.Context(), "post-proxy-reboot", hookCtx); err != nil {
		log.Warn("post-proxy-reboot hook failed: %v", err)
	}

	if len(bootErrors) > 0 {
		return fmt.Errorf("proxy boot failed on %d host(s): %s", len(bootErrors), strings.Join(bootErrors, "; "))
	}

	log.Success("Proxy boot complete")
	return nil
}

func runProxyStop(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getTargetHosts(proxyHost)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	manager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

	for _, host := range hosts {
		if err := manager.Stop(host); err != nil {
			log.HostError(host, "failed to stop proxy: %v", err)
			continue
		}
	}

	log.Success("Proxy stopped")
	return nil
}

func runProxyReboot(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getTargetHosts(proxyHost)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	// Run pre-proxy-reboot hook
	hooks := newHookRunner()
	hookCtx := newHookContext()
	hookCtx.Hosts = strings.Join(hosts, ",")
	if err := hooks.Run(cmd.Context(), "pre-proxy-reboot", hookCtx); err != nil {
		return fmt.Errorf("pre-proxy-reboot hook failed: %w", err)
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	manager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

	var rebootErrors []string
	var succeededHosts []string
	for _, host := range hosts {
		if err := manager.Reboot(host); err != nil {
			log.HostError(host, "failed to reboot proxy: %v", err)
			rebootErrors = append(rebootErrors, fmt.Sprintf("%s: %v", host, err))
			continue
		}
		succeededHosts = append(succeededHosts, host)
	}

	if len(rebootErrors) == len(hosts) {
		return fmt.Errorf("proxy reboot failed on all hosts: %s", strings.Join(rebootErrors, "; "))
	}

	// Run post-proxy-reboot hook (only with succeeded hosts)
	hookCtx.Hosts = strings.Join(succeededHosts, ",")
	if err := hooks.Run(cmd.Context(), "post-proxy-reboot", hookCtx); err != nil {
		log.Warn("post-proxy-reboot hook failed: %v", err)
	}

	if len(rebootErrors) > 0 {
		return fmt.Errorf("proxy reboot failed on %d host(s): %s", len(rebootErrors), strings.Join(rebootErrors, "; "))
	}

	log.Success("Proxy rebooted")
	return nil
}

func runProxyLogs(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	// For logs, we need a single host
	host := proxyHost
	if host == "" {
		hosts := cfg.GetAllHosts()
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts configured")
		}
		host = hosts[0]
		if len(hosts) > 1 {
			log.Warn("Multiple hosts configured, showing logs from %s", host)
		}
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	manager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

	result, err := manager.Logs(host, proxyFollow, proxyTail)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	fmt.Print(result.Stdout)
	if result.Stderr != "" {
		fmt.Print(result.Stderr)
	}

	return nil
}

func runProxyStatus(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getTargetHosts(proxyHost)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	manager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

	log.Header("Proxy Status")

	var rows [][]string
	for _, host := range hosts {
		status, err := manager.Status(host)
		if err != nil {
			rows = append(rows, []string{host, "error", err.Error()})
			continue
		}

		state := "stopped"
		if status.Running {
			state = "running"
		}

		routes := fmt.Sprintf("%d routes", status.RouteCount)
		rows = append(rows, []string{host, state, routes})
	}

	log.Table([]string{"Host", "Status", "Routes"}, rows)
	return nil
}

func runProxyRemove(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getTargetHosts(proxyHost)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	manager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

	for _, host := range hosts {
		if err := manager.Remove(host); err != nil {
			log.HostError(host, "failed to remove proxy: %v", err)
			continue
		}
	}

	log.Success("Proxy removed")
	return nil
}

// getTargetHosts returns the hosts to operate on
func getTargetHosts(specificHost string) []string {
	if specificHost != "" {
		return []string{specificHost}
	}
	return cfg.GetAllHosts()
}
